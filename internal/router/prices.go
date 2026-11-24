package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
)

var embeddedPrices = pricing.Embedded()

// pricingProviders are the models.dev providers whose prices are kept.
var pricingProviders = []string{"anthropic", "openai"}

// rebuildPricesLocked layers config overrides over the last models.dev sync
// over the embedded snapshot.
func (r *Router) rebuildPricesLocked() {
	overrides := map[string]pricing.Price{}
	for k, v := range r.cfg.Prices {
		overrides[k] = pricing.Price{Rates: v}
	}
	r.prices = pricing.Merge(r.synced, embeddedPrices).WithOverrides(overrides)
}

// syncPrices refreshes the table from models.dev when the last sync is older
// than pricing.sync_interval. A failure keeps the current table and is
// retried after an hour at most.
func (r *Router) syncPrices(ctx context.Context, now time.Time) {
	r.mu.Lock()
	pc := r.cfg.Pricing
	due := r.fetcher != nil && pc.Sync &&
		now.Sub(r.synced.SyncedAt) >= pc.SyncInterval &&
		now.Sub(r.pricingTriedAt) >= min(pc.SyncInterval, time.Hour)
	if due {
		r.pricingTriedAt = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	table, err := r.fetchPrices(ctx, pc.URL)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.pricingErr != err.Error() && r.log != nil {
			r.log("warn", "fefo-router price sync failed; keeping the current table", map[string]any{"url": pc.URL, "error": err.Error()})
		}
		r.pricingErr = err.Error()
		return
	}
	table.SyncedAt = now
	r.synced, r.pricingErr = table, ""
	r.rebuildPricesLocked()
	r.dirty = true
}

func (r *Router) fetchPrices(ctx context.Context, url string) (pricing.Table, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := r.fetcher.HTTPDo(ctx, pluginapi.HTTPRequest{
		Method: http.MethodGet, URL: url, Headers: http.Header{"Accept": {"application/json"}},
	})
	if err != nil {
		return pricing.Table{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return pricing.Table{}, fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	t, err := pricing.ParseModelsDev(resp.Body, pricingProviders...)
	if err != nil {
		return pricing.Table{}, err
	}
	if len(t.Models) == 0 {
		return pricing.Table{}, errors.New("no priced anthropic or openai models in the response")
	}
	return t, nil
}

// PricingView describes the price table in the status report.
type PricingView struct {
	Source    string    `json:"source"`
	SyncedAt  time.Time `json:"synced_at,omitzero"`
	Models    int       `json:"models"`
	Overrides int       `json:"overrides,omitempty"`
	SyncError string    `json:"sync_error,omitempty"`
}

func (r *Router) pricingViewLocked() PricingView {
	return PricingView{
		Source: r.prices.Source, SyncedAt: r.prices.SyncedAt, Models: len(r.prices.Models),
		Overrides: len(r.prices.Overrides), SyncError: r.pricingErr,
	}
}
