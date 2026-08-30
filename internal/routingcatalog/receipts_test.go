package routingcatalog

import "testing"

func TestRolloutWithoutRequiredRegionsDoesNotActivateRejectedReceipt(t *testing.T) {
	status := rolloutStatus(nil, []RolloutReceipt{{Status: ReceiptRejected, Region: "us-west"}})
	if status != PublicationFailed {
		t.Fatalf("status = %q, want %q", status, PublicationFailed)
	}
	status = rolloutStatus(nil, []RolloutReceipt{{Status: ReceiptApplied, Region: "us-west"}})
	if status != PublicationActive {
		t.Fatalf("status = %q, want %q", status, PublicationActive)
	}
}
