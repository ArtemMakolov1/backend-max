package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexwordstat"
)

var (
	ErrWordstatNotConfigured = errors.New("wordstat integration for Yandex is not configured")
	ErrWordstatProvider      = errors.New("provider request to Yandex Wordstat failed")
)

const (
	wordstatCacheTTL           = 10 * time.Minute
	wordstatCacheMaxEntries    = 128
	wordstatSuggestionMaxRunes = yandexwordstat.MaxResponsePhraseRunes
	wordstatDefaultQuotaKey    = "maxstudio-default-wordstat-provider"
)

type WordstatProvider interface {
	GetTop(context.Context, yandexwordstat.TopRequest) (yandexwordstat.TopResult, error)
}

type WordstatQuotaStore interface {
	AcquireWordstatQuota(context.Context, string, string, string, time.Time) error
}

// WordstatRateLimitError represents either an internal fair-use quota or an
// authoritative 429 from Yandex. It contains no provider response body or
// request identifier and is safe to turn into a public HTTP 429.
type WordstatRateLimitError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *WordstatRateLimitError) Error() string {
	if e == nil {
		return "Wordstat rate limit exceeded"
	}
	return fmt.Sprintf("Wordstat %s rate limit exceeded", e.Reason)
}

type wordstatCacheEntry struct {
	result    yandexwordstat.TopResult
	fetchedAt time.Time
	expiresAt time.Time
}

type wordstatTopValue struct {
	result    yandexwordstat.TopResult
	fetchedAt time.Time
}

type DirectKeywordSuggestionItem struct {
	Phrase string
	Count  int64
	Source string
}

type DirectKeywordSuggestions struct {
	CampaignID string
	Phrase     string
	TotalCount int64
	Regions    []string
	Items      []DirectKeywordSuggestionItem
	FetchedAt  time.Time
}

func (a *App) ConfigureWordstat(provider WordstatProvider) error {
	return a.ConfigureWordstatWithProviderKey(provider, wordstatDefaultQuotaKey)
}

// ConfigureWordstatWithProviderKey fingerprints the key for cross-replica
// quota coordination. The plaintext key is not retained by App or Store.
func (a *App) ConfigureWordstatWithProviderKey(provider WordstatProvider, providerKey string) error {
	if a == nil {
		return errors.New("application is required")
	}
	if provider == nil {
		return errors.New("provider for Yandex Wordstat is required")
	}
	providerKey = strings.TrimSpace(providerKey)
	if providerKey == "" {
		return errors.New("provider key for Yandex Wordstat quota is required")
	}
	digest := sha256.Sum256([]byte(providerKey))
	a.wordstat = provider
	a.wordstatProviderKeyHash = hex.EncodeToString(digest[:])
	if a.wordstatQuota == nil {
		a.wordstatQuota = a.store
	}
	a.wordstatCacheMu.Lock()
	a.wordstatCache = make(map[string]wordstatCacheEntry)
	a.wordstatCacheMu.Unlock()
	return nil
}

func (a *App) WordstatConfigured() bool {
	return a != nil && a.wordstat != nil
}

func (a *App) SuggestDirectCampaignKeywords(
	ctx context.Context, actorUserID, workspaceID, campaignID, phrase string, limit int,
) (DirectKeywordSuggestions, error) {
	if !a.WordstatConfigured() {
		return DirectKeywordSuggestions{}, ErrWordstatNotConfigured
	}
	if !a.DirectConfigured() || a.directGraph == nil {
		return DirectKeywordSuggestions{}, ErrDirectGraphUnsupported
	}
	phrase = strings.Join(strings.Fields(phrase), " ")
	if phrase == "" || utf8.RuneCountInString(phrase) > 400 || limit < 1 || limit > 50 {
		return DirectKeywordSuggestions{}, fmt.Errorf(
			"%w: phrase must contain 1 to 400 characters and limit must be between 1 and 50",
			store.ErrDirectValidation,
		)
	}
	campaign, err := a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return DirectKeywordSuggestions{}, err
	}
	connection, err := a.store.GetDirectConnection(ctx, actorUserID, workspaceID)
	if err != nil || connection.ID != campaign.ConnectionID || connection.Status != "active" ||
		connection.RevokedAt != nil {
		if err == nil {
			err = store.ErrDirectConnectionRequired
		}
		return DirectKeywordSuggestions{}, err
	}
	token, err := a.directAccessToken(ctx, connection)
	if err != nil {
		return DirectKeywordSuggestions{}, err
	}
	regions, err := a.directGraph.ResolveRegionNames(
		ctx, token, connection.ClientLogin, campaign.Regions,
	)
	if err != nil {
		return DirectKeywordSuggestions{}, a.directGraphProviderError(ctx, connection, err)
	}
	regionIDs := make([]string, 0, len(regions))
	for _, region := range regions {
		if region.ID <= 0 {
			return DirectKeywordSuggestions{}, fmt.Errorf(
				"%w: Direct returned an invalid Wordstat region", ErrDirectSnapshotMismatch,
			)
		}
		regionIDs = append(regionIDs, strconv.FormatInt(region.ID, 10))
	}
	providerResult, fetchedAt, err := a.getWordstatTop(ctx, actorUserID, workspaceID, yandexwordstat.TopRequest{
		Phrase: phrase, Limit: limit, Regions: regionIDs,
	})
	if err != nil {
		return DirectKeywordSuggestions{}, err
	}
	items := directKeywordSuggestionItems(providerResult, limit)
	return DirectKeywordSuggestions{
		CampaignID: campaign.ID, Phrase: phrase, TotalCount: providerResult.TotalCount,
		Regions: append([]string(nil), campaign.Regions...), Items: items,
		FetchedAt: fetchedAt,
	}, nil
}

func (a *App) getWordstatTop(
	ctx context.Context, actorUserID, workspaceID string, request yandexwordstat.TopRequest,
) (yandexwordstat.TopResult, time.Time, error) {
	request = normalizeWordstatRequest(request)
	cacheKey := wordstatTenantCacheKey(
		a.wordstatProviderKeyHash, actorUserID, workspaceID, request,
	)
	flightKey := cacheKey
	now := a.wordstatNow()
	if entry, ok := a.cachedWordstatResult(cacheKey, now); ok {
		return entry.result, entry.fetchedAt, nil
	}

	resultChannel := a.wordstatRequests.DoChan(flightKey, func() (any, error) {
		now := a.wordstatNow()
		if entry, ok := a.cachedWordstatResult(cacheKey, now); ok {
			return wordstatTopValue{result: entry.result, fetchedAt: entry.fetchedAt}, nil
		}
		if a.wordstatQuota == nil || a.wordstatProviderKeyHash == "" {
			return wordstatTopValue{}, errors.New("wordstat quota is not configured")
		}
		if err := a.wordstatQuota.AcquireWordstatQuota(
			ctx, a.wordstatProviderKeyHash, actorUserID, workspaceID, now,
		); err != nil {
			var quotaErr *store.WordstatQuotaError
			if errors.As(err, &quotaErr) {
				return wordstatTopValue{}, &WordstatRateLimitError{
					Reason: quotaErr.Reason, RetryAfter: positiveWordstatRetryAfter(quotaErr.RetryAfter),
				}
			}
			return wordstatTopValue{}, fmt.Errorf("reserve Wordstat quota: %w", err)
		}
		result, err := a.wordstat.GetTop(ctx, request)
		if err != nil {
			var providerErr *yandexwordstat.Error
			if errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusTooManyRequests {
				return wordstatTopValue{}, &WordstatRateLimitError{
					Reason: "provider", RetryAfter: positiveWordstatRetryAfter(providerErr.RetryAfter),
				}
			}
			return wordstatTopValue{}, fmt.Errorf("%w: %w", ErrWordstatProvider, err)
		}
		result = boundedWordstatResult(result, request.Limit)
		fetchedAt := a.wordstatNow()
		a.cacheWordstatResult(cacheKey, result, fetchedAt)
		return wordstatTopValue{result: cloneWordstatResult(result), fetchedAt: fetchedAt}, nil
	})
	var value any
	var resultErr error
	select {
	case <-ctx.Done():
		return yandexwordstat.TopResult{}, time.Time{}, ctx.Err()
	case singleflightResult := <-resultChannel:
		value, resultErr = singleflightResult.Val, singleflightResult.Err
	}
	if resultErr != nil {
		return yandexwordstat.TopResult{}, time.Time{}, resultErr
	}
	top, ok := value.(wordstatTopValue)
	if !ok {
		return yandexwordstat.TopResult{}, time.Time{}, errors.New("invalid Wordstat cache value")
	}
	return cloneWordstatResult(top.result), top.fetchedAt, nil
}

func directKeywordSuggestionItems(
	providerResult yandexwordstat.TopResult, limit int,
) []DirectKeywordSuggestionItem {
	items := make([]DirectKeywordSuggestionItem, 0, limit)
	seen := make(map[string]struct{}, limit)
	appendItems := func(values []yandexwordstat.Phrase, source string) {
		for _, value := range values {
			if len(items) >= limit {
				return
			}
			value.Phrase = normalizeWordstatPhrase(value.Phrase)
			key := strings.ToLower(value.Phrase)
			if value.Phrase == "" || utf8.RuneCountInString(value.Phrase) > wordstatSuggestionMaxRunes ||
				value.Count < 0 {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, DirectKeywordSuggestionItem{
				Phrase: value.Phrase, Count: value.Count, Source: source,
			})
		}
	}
	appendItems(providerResult.Results, "included")
	appendItems(providerResult.Associations, "association")
	return items
}

func normalizeWordstatRequest(request yandexwordstat.TopRequest) yandexwordstat.TopRequest {
	request.Phrase = strings.Join(strings.Fields(request.Phrase), " ")
	seen := make(map[string]struct{}, len(request.Regions))
	regions := make([]string, 0, len(request.Regions))
	for _, region := range request.Regions {
		region = strings.TrimSpace(region)
		if region == "" {
			continue
		}
		if _, duplicate := seen[region]; duplicate {
			continue
		}
		seen[region] = struct{}{}
		regions = append(regions, region)
	}
	sort.Strings(regions)
	request.Regions = regions
	return request
}

func wordstatRequestCacheKey(request yandexwordstat.TopRequest) string {
	canonical := struct {
		Phrase  string   `json:"phrase"`
		Limit   int      `json:"limit"`
		Regions []string `json:"regions"`
	}{
		Phrase: strings.ToLower(request.Phrase), Limit: request.Limit,
		Regions: request.Regions,
	}
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func wordstatTenantCacheKey(
	providerKeyHash, actorUserID, workspaceID string, request yandexwordstat.TopRequest,
) string {
	// Keep cached result timestamps and quota bypasses inside the same tenant
	// and actor boundary. Even though Wordstat results are provider-owned data,
	// sharing a fetched_at value across tenants would reveal that an identical
	// phrase was recently requested elsewhere.
	value := providerKeyHash + "\x00" + workspaceID + "\x00" + actorUserID + "\x00" +
		wordstatRequestCacheKey(request)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (a *App) wordstatNow() time.Time {
	if a != nil && a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (a *App) cachedWordstatResult(key string, now time.Time) (wordstatCacheEntry, bool) {
	a.wordstatCacheMu.Lock()
	defer a.wordstatCacheMu.Unlock()
	entry, ok := a.wordstatCache[key]
	if !ok {
		return wordstatCacheEntry{}, false
	}
	if !now.Before(entry.expiresAt) {
		delete(a.wordstatCache, key)
		return wordstatCacheEntry{}, false
	}
	entry.result = cloneWordstatResult(entry.result)
	return entry, true
}

func (a *App) cacheWordstatResult(key string, result yandexwordstat.TopResult, fetchedAt time.Time) {
	a.wordstatCacheMu.Lock()
	defer a.wordstatCacheMu.Unlock()
	if a.wordstatCache == nil {
		a.wordstatCache = make(map[string]wordstatCacheEntry)
	}
	for cachedKey, entry := range a.wordstatCache {
		if !fetchedAt.Before(entry.expiresAt) {
			delete(a.wordstatCache, cachedKey)
		}
	}
	if len(a.wordstatCache) >= wordstatCacheMaxEntries {
		oldestKey := ""
		var oldest time.Time
		for cachedKey, entry := range a.wordstatCache {
			if oldestKey == "" || entry.fetchedAt.Before(oldest) {
				oldestKey, oldest = cachedKey, entry.fetchedAt
			}
		}
		delete(a.wordstatCache, oldestKey)
	}
	a.wordstatCache[key] = wordstatCacheEntry{
		result: cloneWordstatResult(result), fetchedAt: fetchedAt,
		expiresAt: fetchedAt.Add(wordstatCacheTTL),
	}
}

func cloneWordstatResult(result yandexwordstat.TopResult) yandexwordstat.TopResult {
	result.Results = append([]yandexwordstat.Phrase(nil), result.Results...)
	result.Associations = append([]yandexwordstat.Phrase(nil), result.Associations...)
	return result
}

func boundedWordstatResult(result yandexwordstat.TopResult, limit int) yandexwordstat.TopResult {
	if limit < 1 {
		return yandexwordstat.TopResult{TotalCount: result.TotalCount}
	}
	result.Results = boundedWordstatPhrases(result.Results, limit)
	result.Associations = boundedWordstatPhrases(result.Associations, limit)
	return result
}

func boundedWordstatPhrases(values []yandexwordstat.Phrase, limit int) []yandexwordstat.Phrase {
	result := make([]yandexwordstat.Phrase, 0, min(limit, len(values)))
	for _, value := range values {
		if len(result) >= limit {
			break
		}
		value.Phrase = normalizeWordstatPhrase(value.Phrase)
		if value.Phrase == "" || utf8.RuneCountInString(value.Phrase) > wordstatSuggestionMaxRunes ||
			value.Count < 0 {
			continue
		}
		result = append(result, value)
	}
	return result
}

func normalizeWordstatPhrase(value string) string {
	return strings.Join(strings.Fields(norm.NFC.String(value)), " ")
}

func positiveWordstatRetryAfter(value time.Duration) time.Duration {
	if value < time.Second {
		return time.Second
	}
	return value
}
