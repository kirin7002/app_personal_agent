package contract_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kirin7002/app_personal_agent/contract"
)

// The fake reproduces only the provider's final-transcript boundary.
type fakeSession struct {
	contract.S2SSession
}

func (fakeSession) Commit(context.Context) (contract.FinalTranscript, error) {
	return contract.FinalTranscript{ProviderSessionID: "Q1", InputItemID: "I1", Text: "把车窗关一下"}, nil
}

func TestFakeTranscript(t *testing.T) {
	var session contract.S2SSession = fakeSession{}
	final, err := session.Commit(context.Background())
	if err != nil || final.InputItemID == "" || strings.TrimSpace(final.Text) == "" {
		t.Fatalf("expected correlated final transcript, got %+v, %v", final, err)
	}
}
