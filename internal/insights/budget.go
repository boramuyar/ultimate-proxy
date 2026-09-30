package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// KindBudget is a budget that has passed 80% or 100% of its amount.
const KindBudget = "budget_threshold"

// BudgetAlert is a budget crossing a threshold.
type BudgetAlert struct {
	Rule store.Limit
	// User is the end user a per-user budget crossed for, else empty.
	User     string
	Used     float64 // in the rule's unit (USD for USD budgets)
	ResetsAt time.Time
	// Amount and UsedText are the limit and use written with their unit.
	Amount, UsedText string
}

func budgetKey(ruleID, user string, resets time.Time) string {
	return ruleID + "|" + user + "|" + resets.UTC().Format(time.RFC3339)
}

// Budget opens an insight for a budget passing 80%, or raises it to
// critical at 100%. It resolves when the budget's period ends.
func (e *Engine) Budget(ctx context.Context, a BudgetAlert) {
	now := time.Now().UTC()
	share := a.Used / a.Rule.Amount
	key := budgetKey(a.Rule.ID, a.User, a.ResetsAt)
	who := "the " + a.Rule.Scope()
	if a.User != "" {
		who = a.User
	}
	what := "token"
	if a.Rule.Kind == store.LimitBudgetUSD {
		what = "spend"
	}
	adj := map[string]string{"day": "Daily", "week": "Weekly", "month": "Monthly"}[a.Rule.Period]
	this := map[string]string{"day": "today", "week": "this week", "month": "this month"}[a.Rule.Period]
	severity := "warning"
	title := fmt.Sprintf("%s %s budget for %s is %.0f%% used", adj, what, who, share*100)
	detail := fmt.Sprintf("%s of %s used %s. The budget resets at %s UTC.", a.UsedText, a.Amount, this, a.ResetsAt.UTC().Format("Mon 2 Jan 15:04"))
	if share >= 1 {
		severity = "critical"
		if a.Rule.Enforcement == "hard" {
			detail += " Requests are refused with 429 budget_exceeded until then."
		} else {
			detail += " The budget is soft, so requests are still allowed."
		}
	} else if a.Rule.Enforcement == "hard" {
		detail += " At 100% requests will be refused until it resets."
	}

	e.mu.Lock()
	in := e.budgets[key]
	event := "opened"
	if in == nil {
		in = &store.Insight{
			ID: openresponses.NewID("ins"), Kind: KindBudget, Status: "open",
			TenantID: a.Rule.TenantID, AppID: a.Rule.AppID, FirstSeen: now,
		}
		e.budgets[key] = in
		metrics.InsightsOpen.WithLabelValues(KindBudget).Inc()
	} else if in.Severity == severity {
		event = ""
	} else {
		event = "escalated"
	}
	in.Severity, in.Title, in.Detail, in.LastSeen = severity, title, detail, now
	in.Evidence = map[string]any{
		"limit_id": a.Rule.ID, "user": a.User, "kind": a.Rule.Kind, "period": a.Rule.Period,
		"enforcement": a.Rule.Enforcement, "amount": a.Rule.Amount, "used": round(a.Used),
		"share": round(share), "resets_at": a.ResetsAt.UTC().Format(time.RFC3339),
	}
	snap := *in
	e.mu.Unlock()

	if err := e.store.SaveInsight(ctx, &snap); err != nil {
		e.log.Error("saving insight failed", "err", err, "kind", KindBudget)
	}
	if event != "" {
		e.log.Info("insight "+event, "kind", KindBudget, "tenant", snap.TenantID, "title", snap.Title)
		e.notify.Notify(event, snap)
	}
}

// resolveBudgets closes budget insights whose period has ended; called
// with e.mu held.
func (e *Engine) resolveBudgets(now time.Time) []store.Insight {
	var done []store.Insight
	for key, in := range e.budgets {
		resets, _ := time.Parse(time.RFC3339, fmt.Sprint(in.Evidence["resets_at"]))
		if now.Before(resets) {
			continue
		}
		delete(e.budgets, key)
		at := now.UTC()
		in.Status, in.ResolvedAt, in.LastSeen = "resolved", &at, at
		metrics.InsightsOpen.WithLabelValues(KindBudget).Dec()
		done = append(done, *in)
	}
	return done
}
