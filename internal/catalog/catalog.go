// Package catalog resolves per-model pricing for cost rollups. The source of
// truth is the models.dev community catalog (Apache-2.0); sandbar embeds a
// build-time snapshot so cost rollups work fully offline, and can refresh
// from https://models.dev/api.json on demand — an explicit user action, in
// keeping with the no-network-by-default rule.
//
// Pricing units: USD per million tokens.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aetherbird/sandbar/internal/llm"
)

// CatalogURL is the refresh endpoint.
const CatalogURL = "https://models.dev/api.json"

// fetchTimeout bounds a refresh.
const fetchTimeout = 10 * time.Second

// Cost is per-million-token pricing for one model.
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Limit is a model's context/output budget in tokens.
type Limit struct {
	Context int `json:"context,omitempty"`
	Output  int `json:"output,omitempty"`
}

// Model is one catalog entry.
type Model struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Limit  Limit  `json:"limit,omitempty"`
	Cost   Cost   `json:"cost,omitempty"`
	hidden bool   // alias names excluded from listings
}

// Free reports whether the model has no priced token class — cost segments
// for it are hidden rather than showing a meaningless "$0".
func (m *Model) Free() bool {
	return m == nil || (m.Cost.Input == 0 && m.Cost.Output == 0 &&
		m.Cost.CacheRead == 0 && m.Cost.CacheWrite == 0)
}

// Provider is one provider's models.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name,omitempty"`
	Models map[string]Model `json:"models"`
}

// Catalog maps provider id → provider (the models.dev shape).
type Catalog struct {
	Providers map[string]Provider `json:"-"`
	// Source records where the data came from ("embedded" or a URL).
	Source string
}

// --- wire types (raw JSON) -------------------------------------------------

type wireProvider struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Models map[string]wireModel `json:"models"`
}

type wireModel struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Limit  wireLimit `json:"limit"`
	Cost   wireCost  `json:"cost"`
	Hidden *bool     `json:"hidden"`
}

type wireLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type wireCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Parse decodes models.dev-shaped JSON into a Catalog.
func Parse(data []byte) (*Catalog, error) {
	var raw map[string]wireProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
	c := &Catalog{Providers: map[string]Provider{}}
	for pid, p := range raw {
		if p.ID == "" {
			p.ID = pid
		}
		prov := Provider{ID: p.ID, Name: p.Name, Models: map[string]Model{}}
		for mid, m := range p.Models {
			if m.ID == "" {
				m.ID = mid
			}
			hidden := m.Hidden != nil && *m.Hidden
			prov.Models[mid] = Model{
				ID:     m.ID,
				Name:   m.Name,
				Limit:  Limit{Context: m.Limit.Context, Output: m.Limit.Output},
				Cost:   Cost{Input: m.Cost.Input, Output: m.Cost.Output, CacheRead: m.Cost.CacheRead, CacheWrite: m.Cost.CacheWrite},
				hidden: hidden,
			}
		}
		c.Providers[pid] = prov
	}
	return c, nil
}

// Embedded returns the build-time snapshot.
func Embedded() *Catalog {
	c, err := Parse(snapshotJSON)
	if err != nil {
		// Unreachable: the snapshot is go:embed'ed from a generated file
		// that Parse must accept; fail loud in dev, empty at runtime.
		return &Catalog{Providers: map[string]Provider{}, Source: "embedded-unparsable"}
	}
	c.Source = "embedded"
	return c
}

// Size returns the number of models across all providers.
func (c *Catalog) Size() int {
	if c == nil {
		return 0
	}
	n := 0
	for _, p := range c.Providers {
		n += len(p.Models)
	}
	return n
}

// Lookup finds a model by provider id and model id (either the catalog key
// or the model's own ID field). Unknown providers or models return nil.
func (c *Catalog) Lookup(providerID, modelID string) *Model {
	if c == nil || providerID == "" || modelID == "" {
		return nil
	}
	// Normalize provider aliases that sandbar configs commonly use.
	p, ok := c.Providers[providerID]
	if !ok {
		if norm := normalizeProvider(providerID); norm != providerID {
			p, ok = c.Providers[norm]
		}
	}
	if !ok {
		return nil
	}
	if m, ok := p.Models[modelID]; ok {
		return &m
	}
	// Fall back to matching on the model's ID field (alias keys).
	for _, m := range p.Models {
		if m.ID == modelID {
			return &m
		}
	}
	return nil
}

// normalizeProvider maps a few common config spellings onto catalog ids.
func normalizeProvider(p string) string {
	switch {
	case strings.Contains(p, "anthropic"):
		return "anthropic"
	case strings.Contains(p, "openai"):
		return "openai"
	case strings.Contains(p, "deepseek"):
		return "deepseek"
	case strings.Contains(p, "google"), strings.Contains(p, "gemini"):
		return "google"
	case strings.Contains(p, "mistral"):
		return "mistral"
	case strings.Contains(p, "meta"), strings.Contains(p, "llama"):
		return "meta"
	}
	return p
}

// Refresh fetches the live catalog (explicit user action only —
// `sandbar catalog refresh`). ctx bounds the fetch.
func Refresh(ctx context.Context, url string) (*Catalog, error) {
	if url == "" {
		url = CatalogURL
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: fetch %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("catalog: read: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	c.Source = url
	return c, nil
}

// CostOf computes the USD cost of one provider call's usage. Cache tokens
// are billed at their own rates (and are NOT also billed as input); an
// unknown model returns 0.
func (m *Model) CostOf(u llm.CompletionUsage) float64 {
	if m == nil {
		return 0
	}
	billedInput := u.PromptTokens - u.CacheReadTokens - u.CacheWriteTokens
	if billedInput < 0 {
		billedInput = 0
	}
	return float64(billedInput)*m.Cost.Input/1e6 +
		float64(u.CompletionTokens)*m.Cost.Output/1e6 +
		float64(u.CacheReadTokens)*m.Cost.CacheRead/1e6 +
		float64(u.CacheWriteTokens)*m.Cost.CacheWrite/1e6
}

// FormatUSD renders a cost compactly: sub-cent values in mills.
func FormatUSD(v float64) string {
	switch {
	case v == 0:
		return "$0"
	case v < 0.01:
		return fmt.Sprintf("$%.3f", v) // mills, e.g. $0.004
	case v < 10:
		return fmt.Sprintf("$%.4f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}
