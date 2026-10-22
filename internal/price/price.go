package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const llmPriceUrl = "https://models.dev/api.json"

// 顺序决定同名模型的价格归属：原生厂商在前，先出现者胜出；
// 聚合平台转售的同名模型以 "provider/model" 的形式另存一条。
// 必须与 scripts/updatePrice.py 中的 PROVIDERS 保持一致。
var Provider = []string{
	"openai",     // GPT 系列
	"anthropic",  // Claude 系列
	"google",     // Gemini 系列
	"deepseek",   // DeepSeek 系列
	"xai",        // Grok 系列
	"zhipuai",    // GLM 系列
	"moonshotai", // Kimi/Moonshot
	"minimax",    // MiniMax 系列
	"alibaba",    // Qwen 系列，同时转售 GLM/Kimi 等模型
	"v0",         // v0 系列
}

var lastUpdateTime time.Time

func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()
	client, err := client.GetHTTPClientSystemProxy(false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
	}
	var rawPrice map[string]providerModels
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if err := json.Unmarshal(body, &rawPrice); err != nil {
		return fmt.Errorf("failed to parse LLM info: %w", err)
	}
	llmPriceLock.Lock()
	mergeProviderPrices(llmPrice, rawPrice, Provider)
	llmPriceLock.Unlock()
	lastUpdateTime = time.Now()
	return nil
}

type providerModel struct {
	ID   string         `json:"id"`
	Cost model.LLMPrice `json:"cost"`
}

type providerModels struct {
	Models map[string]providerModel `json:"models"`
}

// mergeProviderPrices 把各 provider 的模型价格合并进 target。
// 同名模型以 providers 中先出现者为准（原生厂商排在聚合平台之前），
// 后出现的转售同名模型以 "provider/model" 为 key 另存，避免互相覆盖。
func mergeProviderPrices(
	target map[string]model.LLMPrice,
	raw map[string]providerModels,
	providers []string,
) {
	// claimed 只记录本轮合并中谁先认领了某个 key。target 里原有的预置项
	// 不参与判定，这样原生厂商仍能覆盖同名预置价格，转售方则走带前缀的 key。
	claimed := make(map[string]struct{}, len(target))
	for _, provider := range providers {
		for _, model := range raw[provider].Models {
			id := strings.ToLower(model.ID)
			if id == "" {
				continue
			}
			key := id
			if _, taken := claimed[key]; taken {
				key = provider + "/" + id
			}
			if _, taken := claimed[key]; taken {
				continue
			}
			claimed[key] = struct{}{}
			target[key] = model.Cost
		}
	}
}

func GetLastUpdateTime() time.Time {
	return lastUpdateTime
}

func GetLLMPrice(modelName string) *model.LLMPrice {
	modelName = strings.ToLower(modelName)
	price, err := op.LLMGet(modelName)
	if err == nil {
		return &price
	}
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	price, ok := llmPrice[modelName]
	if !ok {
		return nil
	}
	return &price
}
