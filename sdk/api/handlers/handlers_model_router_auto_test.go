package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelRouteTargetModelUsesNormalResolutionAndExposesDecision(t *testing.T) {
	host := &handlerRouterOnlyTestHost{hasRouters: true}
	host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
		return pluginapi.ModelRouteResponse{
			Handled: true, TargetKind: pluginapi.ModelRouteTargetModel, TargetModel: "tier-deep",
			Reason: "jev;model=tier-deep;cache_hit=false;confidence=0.9400",
		}, true
	}
	handler := NewBaseAPIHandlers(nil, nil)
	handler.SetModelRouterHost(host)
	decision := handler.applyModelRouter(context.Background(), "openai", "tier-auto", []byte(`{}`), false, modelExecutionOptions{})
	if decision.Model != "tier-deep" || decision.Provider != "" {
		t.Fatalf("route decision = %#v", decision)
	}
	// A model target deliberately leaves Provider empty: normal registry model
	// resolution runs after this decision rather than pinning an executor.
	headers := applyModelRouteHeaders(http.Header{}, decision)
	if headers.Get("X-CPA-Resolved-Model") != "tier-deep" || headers.Get("X-CPA-Route-Confidence") != "0.9400" || headers.Get("X-CPA-Route-Cache-Hit") != "false" {
		t.Fatalf("route headers = %#v", headers)
	}
}
