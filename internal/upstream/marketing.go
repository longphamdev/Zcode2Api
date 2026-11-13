package upstream

// marketing.go — the second plan face.
//
// The Origin distributes claimable plans on two faces: the billing preview
// (billing/preview) and the marketing banner (marketing/touch). A campaign may
// appear on only one of them. The weekend campaign measured 2026-11-13 was
// banner-only: preview answered {"code":0} with no data at all, while the
// banner carried zcode-v3-start-plan-1113-wk.
//
// Shape of a delivery, as observed:
//
//	data.deliveries[].banner.buttons[].action = {type:"claim_zcode_plan", args:{plan_id:"..."}}
//	data.deliveries[].banner.background.args.zcode_plan = {plan_id, name, priority, starts_at, ends_at, entitlements}
//
// The button carries the authoritative plan_id (minimal, schema-conformant);
// the background carries the full plan object for enrichment when the ids
// match. A button id with no matching plan object is still offered — the
// priority lives on the plan object, not on the button, so ranking needs it.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"zcode2api/internal/config"
)

// offeredPlan is one claimable plan, from either face.
type offeredPlan struct {
	PlanID   string
	Name     string
	Priority int
}

// marketingNode is a banner/popup, which may nest buttons.
type marketingNode struct {
	Action *struct {
		Type string `json:"type"`
		Args *struct {
			PlanID string `json:"plan_id"`
		} `json:"args"`
	} `json:"action"`
	Buttons    []marketingNode `json:"buttons"`
	Background json.RawMessage `json:"background"`
}

// marketingDelivery is one entry of data.deliveries.
type marketingDelivery struct {
	CampaignID string         `json:"campaign_id"`
	Banner     *marketingNode `json:"banner"`
	Popup      *marketingNode `json:"popup"`
}

// collectZcodePlans walks a delivery and collects every zcode_plan object it
// finds, depth-limited. The plan object sits under background.args, but its
// exact nesting has moved between campaigns, so the walk is structural rather
// than a fixed path — the same approach the reference client uses.
func collectZcodePlans(node any, out *[]offeredPlan, depth int) {
	if depth > 4 || node == nil {
		return
	}
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			collectZcodePlans(item, out, depth+1)
		}
	case map[string]any:
		if zp, ok := v["zcode_plan"].(map[string]any); ok {
			if p, ok := planFromMap(zp); ok {
				*out = append(*out, p)
			}
		}
		for _, child := range v {
			collectZcodePlans(child, out, depth+1)
		}
	}
}

// planFromMap reads a zcode_plan object.
func planFromMap(m map[string]any) (offeredPlan, bool) {
	id, _ := m["plan_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return offeredPlan{}, false
	}
	p := offeredPlan{PlanID: id, Name: id}
	if n, ok := m["name"].(string); ok && strings.TrimSpace(n) != "" {
		p.Name = strings.TrimSpace(n)
	}
	if pr, ok := m["priority"].(float64); ok {
		p.Priority = int(pr)
	}
	return p, true
}

// parseMarketingDeliveries turns deliveries into claimable plans, deduped by
// plan id.
func parseMarketingDeliveries(deliveries []marketingDelivery) []offeredPlan {
	var plans []offeredPlan
	seen := map[string]bool{}
	for _, d := range deliveries {
		for _, host := range []*marketingNode{d.Banner, d.Popup} {
			if host == nil {
				continue
			}
			// Enrichment: every zcode_plan object inside this banner/popup.
			var rich []offeredPlan
			if len(host.Background) > 0 {
				var bg any
				if json.Unmarshal(host.Background, &bg) == nil {
					collectZcodePlans(bg, &rich, 0)
				}
			}
			richByID := map[string]offeredPlan{}
			for _, p := range rich {
				richByID[p.PlanID] = p
			}
			for _, btn := range host.Buttons {
				if btn.Action == nil || btn.Action.Type != "claim_zcode_plan" {
					continue
				}
				if btn.Action.Args == nil {
					continue
				}
				id := strings.TrimSpace(btn.Action.Args.PlanID)
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				if rich, ok := richByID[id]; ok {
					plans = append(plans, rich)
					continue
				}
				// A button with no matching plan object is still offered.
				plans = append(plans, offeredPlan{PlanID: id, Name: id})
			}
		}
	}
	return plans
}

// marketingPlans fetches the banner face. A failure yields an empty list and
// never blocks enumeration: the billing face stays authoritative when both are
// alive, so a marketing outage must not turn a working preview into "no plans".
func (c *Client) marketingPlans(ctx context.Context, jwt, mid string) []offeredPlan {
	if strings.TrimSpace(mid) == "" {
		return nil // marketing/touch requires a device id
	}
	u := config.OriginBaseURL + "/api/v1/marketing/touch?seq=1"
	headers := http.Header{}
	headers.Set("X-Device-Mid", mid)
	headers.Set("X-Client-Language", "en-US")
	headers.Set("X-ZCode-App-Version", config.BillingClientVersion)
	if jwt != "" {
		headers.Set("Authorization", "Bearer "+jwt)
	}

	status, raw, err := c.getJSON(ctx, http.MethodGet, u, jwt, mid, nil, headers)
	if err != nil || status < 200 || status >= 300 {
		return nil
	}
	var wrap struct {
		Code int `json:"code"`
		Data struct {
			Deliveries []marketingDelivery `json:"deliveries"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil || wrap.Code != 0 {
		return nil
	}
	return parseMarketingDeliveries(wrap.Data.Deliveries)
}
