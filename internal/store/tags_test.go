package store

import "testing"

func TestParseTags(t *testing.T) {
	tags, err := ParseTags(" feature=search , env=prod,user.team=a/b@c:d+e ")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatTags(tags); got != "env=prod,feature=search,user.team=a/b@c:d+e" {
		t.Fatalf("FormatTags = %q", got)
	}
	if tags, err := ParseTags(""); tags != nil || err != nil {
		t.Fatalf("empty header: %v %v", tags, err)
	}
	for _, bad := range []string{
		"feature", "Feature=x", "feature=", "=x", "a=b=c", "a=b,a=c", "a=b c", "a=b,,c=d", `a="b"`,
		"a=b,b=b,c=b,d=b,e=b,f=b,g=b,h=b,i=b,j=b,k=b",
	} {
		if _, err := ParseTags(bad); err == nil {
			t.Errorf("ParseTags(%q) accepted", bad)
		}
	}
}

func TestTagDimension(t *testing.T) {
	q := UsageQuery{GroupBy: []string{"tag:feature"}, Filters: map[string]string{"tag:env": "prod"}}
	q.To = q.From.Add(1)
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"tag:", "tag:Feature", "tag:a'b", "tags"} {
		q := UsageQuery{GroupBy: []string{bad}}
		q.To = q.From.Add(1)
		if q.Validate() == nil {
			t.Errorf("group_by %q accepted", bad)
		}
	}
}
