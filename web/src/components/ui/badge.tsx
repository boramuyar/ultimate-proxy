import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center gap-1 border px-1.5 py-px font-mono text-[10.5px] font-bold uppercase tracking-wider whitespace-nowrap [&>svg]:size-3",
  {
    variants: {
      variant: {
        default: "border-strong bg-foreground text-background",
        outline: "border-strong text-foreground",
        muted: "border-border bg-muted text-muted-foreground",
        good: "border-good/40 bg-good-bg text-good",
        warning: "border-warning/40 bg-warning-bg text-warning",
        critical: "border-critical/40 bg-critical-bg text-critical",
        info: "border-info/40 bg-info-bg text-info",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

function Badge({ className, variant, ...props }: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return <span data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />;
}

export { Badge, badgeVariants };
