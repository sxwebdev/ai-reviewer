package review

import (
	"slices"
	"testing"
)

func TestExtractIdentifiers(t *testing.T) {
	const diff = `@@ -1,4 +1,6 @@
 package payments
-func ProcessRefund(orderID string) error {
+func ProcessRefund(orderID string, amount decimal.Decimal) error {
+	validateAmount(amount)
 	return nil
 }
`
	fd := fileDiff(t, "pay.go", "pay.go", diff)
	got := ExtractIdentifiers([]*FileDiff{fd}, 12)

	for _, want := range []string{"ProcessRefund", "orderID", "validateAmount", "amount", "decimal"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing identifier %q in %v", want, got)
		}
	}
	for _, bad := range []string{"func", "error", "return", "package"} {
		if slices.Contains(got, bad) {
			t.Errorf("stopword %q leaked into %v", bad, got)
		}
	}
	// Context line ("package payments") identifiers must not be extracted.
	if slices.Contains(got, "payments") {
		t.Errorf("context-line identifier extracted: %v", got)
	}
}

func TestExtractIdentifiersCap(t *testing.T) {
	const diff = `@@ -1,1 +1,3 @@
 x
+alpha1 alpha1 alpha1 beta2 beta2 gamma3
+delta4 epsilon5 zeta6 theta7
`
	fd := fileDiff(t, "a.go", "a.go", diff)
	got := ExtractIdentifiers([]*FileDiff{fd}, 3)
	if len(got) != 3 {
		t.Fatalf("cap not applied: %v", got)
	}
	if got[0] != "alpha1" || got[1] != "beta2" {
		t.Errorf("frequency ranking wrong: %v", got)
	}
}
