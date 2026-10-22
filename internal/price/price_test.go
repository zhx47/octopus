package price

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func testProviderModels(models ...providerModel) providerModels {
	result := providerModels{Models: map[string]providerModel{}}
	for _, m := range models {
		result.Models[m.ID] = m
	}
	return result
}

func TestMergeProviderPricesPrefersNativeProvider(t *testing.T) {
	target := map[string]model.LLMPrice{}
	raw := map[string]providerModels{
		"zhipuai": testProviderModels(
			providerModel{ID: "glm-5.2", Cost: model.LLMPrice{Input: 1.19516, Output: 4.18306}},
		),
		"alibaba": testProviderModels(
			providerModel{ID: "glm-5.2", Cost: model.LLMPrice{Input: 1.4, Output: 4.4}},
		),
	}

	mergeProviderPrices(target, raw, []string{"zhipuai", "alibaba"})

	native, ok := target["glm-5.2"]
	if !ok {
		t.Fatalf("missing native key glm-5.2")
	}
	if native.Input != 1.19516 {
		t.Fatalf("native price was overwritten: %+v", native)
	}

	resold, ok := target["alibaba/glm-5.2"]
	if !ok {
		t.Fatalf("missing resold key alibaba/glm-5.2")
	}
	if resold.Input != 1.4 {
		t.Fatalf("unexpected resold price: %+v", resold)
	}
}

func TestMergeProviderPricesKeepsAggregatorOnlyModels(t *testing.T) {
	target := map[string]model.LLMPrice{}
	raw := map[string]providerModels{
		"alibaba": testProviderModels(
			providerModel{ID: "Qwen3-Max", Cost: model.LLMPrice{Input: 2, Output: 6}},
		),
	}

	mergeProviderPrices(target, raw, []string{"alibaba"})

	price, ok := target["qwen3-max"]
	if !ok {
		t.Fatalf("expected lowercased key qwen3-max, got %v", target)
	}
	if price.Input != 2 || price.Output != 6 {
		t.Fatalf("unexpected price: %+v", price)
	}
}

func TestMergeProviderPricesDoesNotDuplicateOnRefresh(t *testing.T) {
	target := map[string]model.LLMPrice{}
	raw := map[string]providerModels{
		"moonshotai": testProviderModels(
			providerModel{ID: "kimi-k3", Cost: model.LLMPrice{Input: 3, Output: 15}},
		),
		"alibaba": testProviderModels(
			providerModel{ID: "kimi-k3", Cost: model.LLMPrice{Input: 3, Output: 15}},
		),
	}
	providers := []string{"moonshotai", "alibaba"}

	mergeProviderPrices(target, raw, providers)
	mergeProviderPrices(target, raw, providers)

	if len(target) != 2 {
		t.Fatalf("expected 2 entries after repeated refresh, got %d: %v", len(target), target)
	}
	if _, ok := target["kimi-k3"]; !ok {
		t.Fatalf("missing native key kimi-k3")
	}
	if _, ok := target["alibaba/kimi-k3"]; !ok {
		t.Fatalf("missing resold key alibaba/kimi-k3")
	}
}
