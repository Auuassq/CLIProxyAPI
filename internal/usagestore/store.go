package usagestore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultCurrency = "USD"
	usageDirName    = "usage"
	usageFileName   = "usage.jsonl"
)

var defaultStore = NewStore()

func init() {
	coreusage.RegisterPlugin(defaultStore)
}

// Store persists usage records as JSONL under auth-dir so upgrades/restarts do not lose statistics.
type Store struct {
	mu      sync.RWMutex
	enabled bool
	path    string
	billing config.BillingConfig
}

// Entry is the persisted shape for one request usage record.
type Entry struct {
	Timestamp     time.Time  `json:"timestamp"`
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Alias         string     `json:"alias,omitempty"`
	Endpoint      string     `json:"endpoint,omitempty"`
	AuthType      string     `json:"auth_type,omitempty"`
	Source        string     `json:"source,omitempty"`
	AuthIndex     string     `json:"auth_index,omitempty"`
	APIKeyHash    string     `json:"api_key_hash,omitempty"`
	APIKeyPreview string     `json:"api_key_preview,omitempty"`
	RequestID     string     `json:"request_id,omitempty"`
	LatencyMs     int64      `json:"latency_ms"`
	Tokens        TokenStats `json:"tokens"`
	Failed        bool       `json:"failed"`
	StatusCode    int        `json:"status_code"`
	FailBody      string     `json:"fail_body,omitempty"`

	EstimatedCost float64 `json:"estimated_cost,omitempty"`
	Priced        bool    `json:"priced,omitempty"`
}

// TokenStats is the token breakdown persisted for usage/billing summaries.
type TokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

// Query controls usage summary filtering.
type Query struct {
	Since       time.Time
	SinceLabel  string
	RecentLimit int
}

// Summary contains persisted usage totals plus model/provider breakdowns.
type Summary struct {
	Enabled     bool      `json:"enabled"`
	StoragePath string    `json:"storage_path,omitempty"`
	Currency    string    `json:"currency"`
	Since       string    `json:"since,omitempty"`
	From        time.Time `json:"from,omitempty"`
	To          time.Time `json:"to"`
	Total       Bucket    `json:"total"`
	ByProvider  []Bucket  `json:"by_provider"`
	ByModel     []Bucket  `json:"by_model"`
	Recent      []Entry   `json:"recent,omitempty"`
}

// Bucket is an aggregate usage/cost row.
type Bucket struct {
	Key           string     `json:"key,omitempty"`
	Provider      string     `json:"provider,omitempty"`
	Model         string     `json:"model,omitempty"`
	Requests      int64      `json:"requests"`
	Success       int64      `json:"success"`
	Failed        int64      `json:"failed"`
	Tokens        TokenStats `json:"tokens"`
	EstimatedCost float64    `json:"estimated_cost"`
	Priced        bool       `json:"priced"`
}

// NewStore constructs an independent usage store.
func NewStore() *Store {
	return &Store{billing: config.BillingConfig{Currency: defaultCurrency}}
}

// Configure updates the default store from application config.
func Configure(cfg *config.Config) {
	if err := defaultStore.Configure(cfg); err != nil {
		log.Warnf("usage store configure failed: %v", err)
	}
}

// GetSummary returns the default store summary.
func GetSummary(query Query) (Summary, error) {
	return defaultStore.Summary(query)
}

// Reset clears the default persisted usage file.
func Reset() error {
	return defaultStore.Reset()
}

// Configure updates this store from application config.
func (s *Store) Configure(cfg *config.Config) error {
	if s == nil {
		return nil
	}
	enabled := cfg != nil && cfg.UsageStatisticsEnabled
	authDir := ""
	billing := config.BillingConfig{Currency: defaultCurrency}
	if cfg != nil {
		authDir = cfg.AuthDir
		billing = normalizeBilling(cfg.Billing)
	}
	resolvedAuthDir, err := util.ResolveAuthDir(authDir)
	if err != nil {
		return err
	}
	path := filepath.Join(resolvedAuthDir, usageDirName, usageFileName)

	s.mu.Lock()
	s.enabled = enabled
	s.path = path
	s.billing = billing
	s.mu.Unlock()

	if enabled {
		return os.MkdirAll(filepath.Dir(path), 0o700)
	}
	return nil
}

// HandleUsage implements usage.Plugin.
func (s *Store) HandleUsage(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	s.mu.RLock()
	enabled := s.enabled
	path := s.path
	s.mu.RUnlock()
	if !enabled || strings.TrimSpace(path) == "" {
		return
	}

	entry := entryFromRecord(ctx, record)
	if err := s.appendEntry(path, entry); err != nil {
		log.Warnf("persist usage record failed: %v", err)
	}
}

func (s *Store) appendEntry(path string, entry Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Debugf("close usage store file failed: %v", errClose)
		}
	}()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// Summary scans persisted usage records and aggregates them for display.
func (s *Store) Summary(query Query) (Summary, error) {
	if s == nil {
		return Summary{}, errors.New("usage store unavailable")
	}
	s.mu.RLock()
	enabled := s.enabled
	path := s.path
	billing := s.billing
	s.mu.RUnlock()

	now := time.Now()
	summary := Summary{
		Enabled:     enabled,
		StoragePath: path,
		Currency:    billingCurrency(billing),
		Since:       query.SinceLabel,
		From:        query.Since,
		To:          now,
	}
	if strings.TrimSpace(path) == "" {
		return summary, nil
	}

	entries, err := readEntries(path, query.Since)
	if err != nil {
		if os.IsNotExist(err) {
			return summary, nil
		}
		return summary, err
	}

	providerBuckets := map[string]*Bucket{}
	modelBuckets := map[string]*Bucket{}
	recentLimit := query.RecentLimit
	if recentLimit < 0 {
		recentLimit = 0
	}

	for i := range entries {
		entry := entries[i]
		cost, priced := estimateEntryCost(entry, billing)
		entry.EstimatedCost = cost
		entry.Priced = priced

		addToBucket(&summary.Total, entry)
		providerKey := strings.ToLower(strings.TrimSpace(entry.Provider))
		if providerKey == "" {
			providerKey = "unknown"
		}
		providerBucket := providerBuckets[providerKey]
		if providerBucket == nil {
			providerBucket = &Bucket{Key: providerKey, Provider: entry.Provider}
			providerBuckets[providerKey] = providerBucket
		}
		addToBucket(providerBucket, entry)

		modelKey := providerKey + "/" + strings.ToLower(strings.TrimSpace(entry.Model))
		modelBucket := modelBuckets[modelKey]
		if modelBucket == nil {
			modelBucket = &Bucket{Key: modelKey, Provider: entry.Provider, Model: entry.Model}
			modelBuckets[modelKey] = modelBucket
		}
		addToBucket(modelBucket, entry)

		if recentLimit > 0 {
			summary.Recent = append(summary.Recent, entry)
		}
	}

	if recentLimit > 0 && len(summary.Recent) > recentLimit {
		summary.Recent = summary.Recent[len(summary.Recent)-recentLimit:]
	}
	reverseEntries(summary.Recent)

	summary.ByProvider = bucketsFromMap(providerBuckets)
	summary.ByModel = bucketsFromMap(modelBuckets)
	return summary, nil
}

// Reset truncates the persisted usage file.
func (s *Store) Reset() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}

func entryFromRecord(ctx context.Context, record coreusage.Record) Entry {
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	tokens := TokenStats{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	normalizeTokenTotal(&tokens)

	provider := defaultString(record.Provider, "unknown")
	model := defaultString(record.Model, "unknown")
	alias := strings.TrimSpace(record.Alias)
	if alias == "" {
		alias = model
	}

	failed := record.Failed
	statusCode := record.Fail.StatusCode
	if !failed {
		status := internallogging.GetResponseStatus(ctx)
		if status >= 400 {
			failed = true
			statusCode = status
		}
	}
	if statusCode <= 0 {
		if failed {
			statusCode = 500
		} else {
			statusCode = 200
		}
	}

	apiKey := strings.TrimSpace(record.APIKey)
	return Entry{
		Timestamp:     timestamp,
		Provider:      provider,
		Model:         model,
		Alias:         alias,
		Endpoint:      strings.TrimSpace(internallogging.GetEndpoint(ctx)),
		AuthType:      defaultString(record.AuthType, "unknown"),
		Source:        strings.TrimSpace(record.Source),
		AuthIndex:     strings.TrimSpace(record.AuthIndex),
		APIKeyHash:    secretHash(apiKey),
		APIKeyPreview: secretPreview(apiKey),
		RequestID:     strings.TrimSpace(internallogging.GetRequestID(ctx)),
		LatencyMs:     record.Latency.Milliseconds(),
		Tokens:        tokens,
		Failed:        failed,
		StatusCode:    statusCode,
		FailBody:      strings.TrimSpace(record.Fail.Body),
	}
}

func readEntries(path string, since time.Time) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Debugf("close usage store file failed: %v", errClose)
		}
	}()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry Entry
		if errUnmarshal := json.Unmarshal([]byte(line), &entry); errUnmarshal != nil {
			continue
		}
		if !since.IsZero() && entry.Timestamp.Before(since) {
			continue
		}
		normalizeTokenTotal(&entry.Tokens)
		entries = append(entries, entry)
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func addToBucket(bucket *Bucket, entry Entry) {
	if bucket == nil {
		return
	}
	bucket.Requests++
	if entry.Failed {
		bucket.Failed++
	} else {
		bucket.Success++
	}
	addTokens(&bucket.Tokens, entry.Tokens)
	bucket.EstimatedCost += entry.EstimatedCost
	if entry.Priced {
		bucket.Priced = true
	}
}

func addTokens(dst *TokenStats, src TokenStats) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.ReasoningTokens += src.ReasoningTokens
	dst.CachedTokens += src.CachedTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.TotalTokens += src.TotalTokens
}

func bucketsFromMap(input map[string]*Bucket) []Bucket {
	out := make([]Bucket, 0, len(input))
	for _, bucket := range input {
		if bucket == nil {
			continue
		}
		out = append(out, *bucket)
	}
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].EstimatedCost-out[j].EstimatedCost) > 0.0000001 {
			return out[i].EstimatedCost > out[j].EstimatedCost
		}
		if out[i].Tokens.TotalTokens != out[j].Tokens.TotalTokens {
			return out[i].Tokens.TotalTokens > out[j].Tokens.TotalTokens
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func reverseEntries(entries []Entry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

func normalizeTokenTotal(tokens *TokenStats) {
	if tokens == nil || tokens.TotalTokens != 0 {
		return
	}
	total := tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	if total == 0 {
		total = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	tokens.TotalTokens = total
}

func normalizeBilling(billing config.BillingConfig) config.BillingConfig {
	billing.Currency = billingCurrency(billing)
	out := make([]config.BillingPrice, 0, len(billing.Prices))
	for _, price := range billing.Prices {
		price.Provider = strings.TrimSpace(price.Provider)
		price.Model = strings.TrimSpace(price.Model)
		if price.Model == "" {
			continue
		}
		out = append(out, price)
	}
	billing.Prices = out
	return billing
}

func billingCurrency(billing config.BillingConfig) string {
	currency := strings.TrimSpace(billing.Currency)
	if currency == "" {
		return defaultCurrency
	}
	return currency
}

func estimateEntryCost(entry Entry, billing config.BillingConfig) (float64, bool) {
	price, ok := matchPrice(entry.Provider, entry.Model, billing.Prices)
	if !ok {
		return 0, false
	}
	if entry.Failed && !price.IncludeFailedRequestTokens {
		if price.RequestPer1K <= 0 || price.SuccessfulRequestOnly {
			return 0, true
		}
		return price.RequestPer1K / 1000, true
	}
	tokens := entry.Tokens
	inputTokens := tokens.InputTokens
	cost := 0.0

	if price.CacheReadInputPer1M > 0 && tokens.CacheReadTokens > 0 {
		inputTokens = subtractNonNegative(inputTokens, tokens.CacheReadTokens)
		cost += perMillion(tokens.CacheReadTokens, price.CacheReadInputPer1M)
	}
	if price.CacheCreationInputPer1M > 0 && tokens.CacheCreationTokens > 0 {
		inputTokens = subtractNonNegative(inputTokens, tokens.CacheCreationTokens)
		cost += perMillion(tokens.CacheCreationTokens, price.CacheCreationInputPer1M)
	}
	if price.CachedInputPer1M > 0 && tokens.CachedTokens > 0 {
		inputTokens = subtractNonNegative(inputTokens, tokens.CachedTokens)
		cost += perMillion(tokens.CachedTokens, price.CachedInputPer1M)
	}

	cost += perMillion(inputTokens, price.InputPer1M)
	cost += perMillion(tokens.OutputTokens, price.OutputPer1M)
	cost += perMillion(tokens.ReasoningTokens, price.ReasoningOutputPer1M)
	if price.RequestPer1K > 0 && (!entry.Failed || !price.SuccessfulRequestOnly) {
		cost += price.RequestPer1K / 1000
	}
	return cost, true
}

func matchPrice(provider, model string, prices []config.BillingPrice) (config.BillingPrice, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	for _, price := range prices {
		priceProvider := strings.ToLower(strings.TrimSpace(price.Provider))
		priceModel := strings.ToLower(strings.TrimSpace(price.Model))
		if priceProvider != "" && priceProvider != "*" && !wildcardMatch(priceProvider, provider) {
			continue
		}
		if wildcardMatch(priceModel, model) {
			return price, true
		}
	}
	return config.BillingPrice{}, false
}

func wildcardMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && !strings.HasPrefix(pattern, "*") && idx != 0 {
			return false
		}
		pos += idx + len(part)
	}
	last := parts[len(parts)-1]
	if last != "" && !strings.HasSuffix(pattern, "*") && !strings.HasSuffix(value, last) {
		return false
	}
	return true
}

func perMillion(tokens int64, price float64) float64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	return float64(tokens) * price / 1_000_000
}

func subtractNonNegative(value, delta int64) int64 {
	if delta <= 0 {
		return value
	}
	if value <= delta {
		return 0
	}
	return value - delta
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func secretHash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func secretPreview(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "****"
	}
	return fmt.Sprintf("%s...%s", value[:4], value[len(value)-4:])
}
