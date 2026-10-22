package sitesync

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// RunAnyTime 是站长私自魔改的 new-api 站点，在签到接口前加了一道 PoW 门槛：
// 先取 challenge，对 prefix + 8 位十六进制 nonce 做 SHA-256，要求摘要前缀有
// difficulty 个 0 位，再把 challenge_id / nonce 作为查询参数提交签到。
// 这只是针对该站的特例适配，其余同步、余额、建 Key 逻辑仍沿用 new-api 家族实现。
const (
	runAnyTimeChallengePath    = "/api/user/pow/challenge"
	runAnyTimeAction           = "checkin"
	runAnyTimeNonceDigits      = 8
	runAnyTimeMaxSolveDuration = 2 * time.Minute
	runAnyTimeMinSolveDuration = 5 * time.Second
	runAnyTimeExpirySafety     = 3 * time.Second
)

// runAnyTimeLeadingBitMasks[i] 匹配一个字节的前 i 位，用于校验 difficulty 的非整字节部分。
var runAnyTimeLeadingBitMasks = [9]uint8{
	0x00, 0x80, 0xC0, 0xE0, 0xF0, 0xF8, 0xFC, 0xFE, 0xFF,
}

type runAnyTimeChallenge struct {
	ChallengeID string
	Prefix      string
	Difficulty  int
	ExpiresAt   int64
}

func checkinRunAnyTime(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount) (*model.SiteCheckinResult, string, error) {
	accessToken, err := resolveManagedAccessToken(ctx, siteRecord, account)
	if err != nil {
		return nil, accessToken, err
	}

	// 站点若去掉了 PoW 门槛会返回 404，此时退回普通 new-api 签到流程。
	result, powAvailable, err := checkinWithRunAnyTime(ctx, siteRecord, account, accessToken)
	if powAvailable {
		if err != nil {
			return &model.SiteCheckinResult{Status: model.SiteExecutionStatusFailed, Message: err.Error()}, accessToken, nil
		}
		return result, accessToken, nil
	}
	return checkinRunAnyTimePlain(ctx, siteRecord, account, accessToken)
}

// checkinWithRunAnyTime 的 available 返回值表示站点是否真的提供了 PoW 挑战接口。
func checkinWithRunAnyTime(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, accessToken string) (*model.SiteCheckinResult, bool, error) {
	challengeURL := buildSiteURL(siteRecord.BaseURL, runAnyTimeChallengePath) + "?action=" + url.QueryEscape(runAnyTimeAction)
	payload, err := requestJSONWithManagedAccessToken(ctx, siteRecord, http.MethodGet, challengeURL, nil, accessToken, account)
	if err != nil {
		if isRunAnyTimeEndpointMissing(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if payload == nil {
		return nil, false, nil
	}
	if !jsonBool(payload["success"]) {
		message := anyRouterExtractResponseMessage(payload)
		if isAlreadyCheckedInMessage(message) {
			return &model.SiteCheckinResult{Status: model.SiteExecutionStatusSuccess, Message: message}, true, nil
		}
		return nil, false, fmt.Errorf("%s", firstNonEmptyString(message, "pow challenge failed"))
	}

	challenge, ok := parseRunAnyTimeChallenge(payload)
	if !ok {
		return nil, false, fmt.Errorf("pow challenge response is malformed")
	}

	nonce, err := solveRunAnyTime(ctx, challenge)
	if err != nil {
		return nil, true, fmt.Errorf("pow solve failed: %w", err)
	}

	checkinURL := fmt.Sprintf(
		"%s?pow_challenge=%s&pow_nonce=%s",
		buildSiteURL(siteRecord.BaseURL, "/api/user/checkin"),
		url.QueryEscape(challenge.ChallengeID),
		url.QueryEscape(nonce),
	)
	checkinPayload, err := requestJSONWithManagedAccessToken(ctx, siteRecord, http.MethodPost, checkinURL, nil, accessToken, account)
	if err != nil {
		return nil, true, err
	}
	return buildRunAnyTimeCheckinResult(checkinPayload), true, nil
}

// checkinRunAnyTimePlain 是站点取消 PoW 后的兜底，复用 new-api 的签到行为。
func checkinRunAnyTimePlain(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, accessToken string) (*model.SiteCheckinResult, string, error) {
	payload, err := requestJSONWithManagedAccessToken(ctx, siteRecord, http.MethodPost, buildSiteURL(siteRecord.BaseURL, "/api/user/checkin"), nil, accessToken, account)
	if err != nil {
		lowered := strings.ToLower(err.Error())
		if strings.Contains(lowered, "404") || strings.Contains(lowered, "not found") {
			return &model.SiteCheckinResult{Status: model.SiteExecutionStatusSkipped, Message: "checkin is not supported by this platform"}, accessToken, nil
		}
		return nil, accessToken, err
	}
	success := jsonBool(payload["success"])
	message := firstNonEmptyString(jsonString(payload["message"]), "checkin success")
	if success || isAlreadyCheckedInMessage(message) {
		return &model.SiteCheckinResult{Status: model.SiteExecutionStatusSuccess, Message: message, Reward: jsonString(nestedValue(payload, "data", "reward"))}, accessToken, nil
	}
	return &model.SiteCheckinResult{Status: model.SiteExecutionStatusFailed, Message: message}, accessToken, nil
}

func isRunAnyTimeEndpointMissing(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(err.Error())
	return strings.Contains(lowered, "http 404") ||
		strings.Contains(lowered, "not found") ||
		strings.Contains(lowered, "invalid url")
}

func parseRunAnyTimeChallenge(payload map[string]any) (runAnyTimeChallenge, bool) {
	data, ok := payload["data"].(map[string]any)
	if !ok {
		return runAnyTimeChallenge{}, false
	}
	challengeID := jsonString(data["challenge_id"])
	prefix, _ := data["prefix"].(string)
	difficulty := int(jsonFloat(data["difficulty"]))
	if challengeID == "" || prefix == "" || difficulty < 0 || difficulty > sha256.Size*8 {
		return runAnyTimeChallenge{}, false
	}
	expiresAt := int64(jsonFloat(data["expires_at"]))
	if expiresAt > 1_000_000_000_000 {
		expiresAt /= 1000
	}
	return runAnyTimeChallenge{
		ChallengeID: challengeID,
		Prefix:      prefix,
		Difficulty:  difficulty,
		ExpiresAt:   expiresAt,
	}, true
}

func solveRunAnyTime(ctx context.Context, challenge runAnyTimeChallenge) (string, error) {
	timeout := runAnyTimeMaxSolveDuration
	if challenge.ExpiresAt > 0 {
		remaining := time.Until(time.Unix(challenge.ExpiresAt, 0)) - runAnyTimeExpirySafety
		if remaining < timeout {
			timeout = remaining
		}
		if timeout < runAnyTimeMinSolveDuration {
			timeout = runAnyTimeMinSolveDuration
		}
	}
	solveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 站点 worker 把 nonce 的 8 位十六进制字符串直接拼在 prefix 后面再哈希，
	// 因此这里也必须哈希 ASCII 字符，而不是 4 字节大端整数。
	prefix := []byte(challenge.Prefix)
	work := make([]byte, len(prefix)+runAnyTimeNonceDigits)
	copy(work, prefix)
	nonceOffset := len(prefix)
	var nonceBytes [4]byte

	for nonce := uint64(0); nonce <= math.MaxUint32; nonce++ {
		if nonce%4096 == 0 {
			select {
			case <-solveCtx.Done():
				return "", solveCtx.Err()
			default:
			}
		}
		binary.BigEndian.PutUint32(nonceBytes[:], uint32(nonce))
		hex.Encode(work[nonceOffset:], nonceBytes[:])
		if runAnyTimeDigestMatches(sha256.Sum256(work), challenge.Difficulty) {
			return string(work[nonceOffset:]), nil
		}
	}
	return "", fmt.Errorf("max attempts reached without solution")
}

func runAnyTimeDigestMatches(digest [sha256.Size]byte, difficulty int) bool {
	if difficulty <= 0 {
		return true
	}
	fullBytes := difficulty / 8
	if fullBytes > len(digest) {
		return false
	}
	for i := 0; i < fullBytes; i++ {
		if digest[i] != 0 {
			return false
		}
	}
	remainder := difficulty % 8
	if remainder == 0 {
		return true
	}
	if fullBytes >= len(digest) {
		return false
	}
	return digest[fullBytes]&runAnyTimeLeadingBitMasks[remainder] == 0
}

func buildRunAnyTimeCheckinResult(payload map[string]any) *model.SiteCheckinResult {
	message := firstNonEmptyString(jsonString(payload["message"]), "checkin success")
	reward := runAnyTimeCheckinReward(payload)
	if jsonBool(payload["success"]) || isAlreadyCheckedInMessage(message) {
		return &model.SiteCheckinResult{Status: model.SiteExecutionStatusSuccess, Message: message, Reward: reward}
	}
	return &model.SiteCheckinResult{Status: model.SiteExecutionStatusFailed, Message: firstNonEmptyString(jsonString(payload["message"]), "checkin failed")}
}

// runAnyTimeCheckinReward 优先使用标准 reward 字段；该站返回的是 quota_awarded
// （额度单位），按 new-api 家族的兑换比例附上美元近似值。
func runAnyTimeCheckinReward(payload map[string]any) string {
	if reward := jsonString(nestedValue(payload, "data", "reward")); reward != "" {
		return reward
	}
	quota := jsonFloat(nestedValue(payload, "data", "quota_awarded"))
	if quota <= 0 {
		return ""
	}
	return fmt.Sprintf("%d（约 $%.2f）", int64(quota), quota/siteBalanceQuotaPerUSD)
}
