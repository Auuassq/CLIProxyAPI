package usagestore

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestStorePersistsAndSummarizesUsageAcrossInstances(t *testing.T) {
	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir:                authDir,
		UsageStatisticsEnabled: true,
		Billing: config.BillingConfig{
			Currency: "USD",
			Prices: []config.BillingPrice{
				{
					Provider:         "openai",
					Model:            "gpt-*",
					InputPer1M:       1,
					OutputPer1M:      2,
					CachedInputPer1M: 0.5,
				},
			},
		},
	}
	requestedAt := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	store := NewStore()
	if err := store.Configure(cfg); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	store.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-5",
		Alias:       "client-gpt",
		APIKey:      "sk-test-secret",
		RequestedAt: requestedAt,
		Detail: coreusage.Detail{
			InputTokens:  1_000_000,
			OutputTokens: 2_000_000,
			CachedTokens: 100_000,
			TotalTokens:  3_000_000,
		},
	})

	reloaded := NewStore()
	if err := reloaded.Configure(cfg); err != nil {
		t.Fatalf("reloaded Configure() error = %v", err)
	}
	summary, err := reloaded.Summary(Query{Since: requestedAt.Add(-time.Hour), RecentLimit: 1})
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}

	if summary.Total.Requests != 1 {
		t.Fatalf("requests = %d, want 1", summary.Total.Requests)
	}
	if summary.Total.Tokens.TotalTokens != 3_000_000 {
		t.Fatalf("total tokens = %d, want 3000000", summary.Total.Tokens.TotalTokens)
	}
	wantCost := 4.95 // (900k input * $1) + (100k cached * $0.5) + (2M output * $2)
	if math.Abs(summary.Total.EstimatedCost-wantCost) > 0.000001 {
		t.Fatalf("estimated cost = %f, want %f", summary.Total.EstimatedCost, wantCost)
	}
	if !summary.Total.Priced {
		t.Fatal("expected summary to be marked priced")
	}
	if len(summary.Recent) != 1 {
		t.Fatalf("recent records = %d, want 1", len(summary.Recent))
	}
	if summary.Recent[0].APIKeyHash == "" || summary.Recent[0].APIKeyPreview == "" {
		t.Fatal("expected persisted API key hash and preview")
	}
	if summary.Recent[0].APIKeyPreview == "sk-test-secret" {
		t.Fatal("API key preview must not store raw key")
	}
}

func TestStoreDisabledDoesNotPersistUsage(t *testing.T) {
	store := NewStore()
	if err := store.Configure(&config.Config{AuthDir: t.TempDir(), UsageStatisticsEnabled: false}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	store.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai",
		Model:    "gpt-5",
		Detail:   coreusage.Detail{TotalTokens: 1},
	})

	summary, err := store.Summary(Query{})
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	if summary.Total.Requests != 0 {
		t.Fatalf("requests = %d, want 0", summary.Total.Requests)
	}
}
