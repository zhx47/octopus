package sitesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestRunAnyTimeDigestMatches(t *testing.T) {
	cases := []struct {
		name       string
		digest     [sha256.Size]byte
		difficulty int
		want       bool
	}{
		{name: "zero difficulty always matches", digest: [sha256.Size]byte{0xFF}, difficulty: 0, want: true},
		{name: "one zero bit matches", digest: [sha256.Size]byte{0x7F}, difficulty: 1, want: true},
		{name: "one zero bit fails", digest: [sha256.Size]byte{0x80}, difficulty: 1, want: false},
		{name: "byte aligned matches", digest: [sha256.Size]byte{0x00, 0x00, 0x10}, difficulty: 16, want: true},
		{name: "byte aligned fails", digest: [sha256.Size]byte{0x00, 0x01}, difficulty: 16, want: false},
		{name: "partial byte matches", digest: [sha256.Size]byte{0x00, 0x1F}, difficulty: 11, want: true},
		{name: "partial byte fails", digest: [sha256.Size]byte{0x00, 0x20}, difficulty: 11, want: false},
		{name: "impossible difficulty fails", digest: [sha256.Size]byte{}, difficulty: 300, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runAnyTimeDigestMatches(tc.digest, tc.difficulty); got != tc.want {
				t.Fatalf("runAnyTimeDigestMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunAnyTimeMatchesReferenceSolution(t *testing.T) {
	// 用站点实际接受过的解做回归：prefix + "00004851" 的 SHA-256 应满足 difficulty 14。
	digest := sha256.Sum256([]byte("checkin:1792565712:6573:57c20d569dc58d08:00004851"))
	if !runAnyTimeDigestMatches(digest, 14) {
		t.Fatalf("reference nonce does not satisfy difficulty 14: %x", digest)
	}
}

func TestSolveRunAnyTimeReturnsMatchingNonce(t *testing.T) {
	challenge := runAnyTimeChallenge{
		Prefix:     "checkin:1792565712:6573:57c20d569dc58d08:",
		Difficulty: 12,
	}
	nonce, err := solveRunAnyTime(context.Background(), challenge)
	if err != nil {
		t.Fatalf("solveRunAnyTime returned error: %v", err)
	}
	if len(nonce) != runAnyTimeNonceDigits {
		t.Fatalf("expected %d-char nonce, got %q", runAnyTimeNonceDigits, nonce)
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		t.Fatalf("expected hexadecimal nonce, got %q", nonce)
	}
	digest := sha256.Sum256([]byte(challenge.Prefix + nonce))
	if !runAnyTimeDigestMatches(digest, challenge.Difficulty) {
		t.Fatalf("nonce %q does not satisfy difficulty %d", nonce, challenge.Difficulty)
	}
}

func TestParseRunAnyTimeChallenge(t *testing.T) {
	payload := map[string]any{
		"success": true,
		"data": map[string]any{
			"challenge_id": "b14e28e0e9478e0d58c4e34a3b4f0ac9",
			"prefix":       "checkin:1792565712:6573:57c20d569dc58d08:",
			"difficulty":   float64(14),
			"expires_at":   float64(1792565732),
		},
	}
	challenge, ok := parseRunAnyTimeChallenge(payload)
	if !ok {
		t.Fatalf("expected challenge to parse")
	}
	if challenge.ChallengeID != "b14e28e0e9478e0d58c4e34a3b4f0ac9" || challenge.Difficulty != 14 || challenge.ExpiresAt != 1792565732 {
		t.Fatalf("unexpected challenge: %+v", challenge)
	}

	invalid := map[string]any{"success": true, "data": map[string]any{"challenge_id": "", "prefix": "x", "difficulty": float64(4)}}
	if _, ok := parseRunAnyTimeChallenge(invalid); ok {
		t.Fatalf("expected empty challenge id to be rejected")
	}
}

// runAnyTimeTestServer 模拟魔改站：普通签到一律要求 PoW，只有带 pow_challenge/pow_nonce
// 且 nonce 真正满足难度的请求才会成功。
func runAnyTimeTestServer(t *testing.T, difficulty int, prefix string) (*httptest.Server, *string, *string) {
	t.Helper()
	submittedChallenge := new(string)
	submittedNonce := new(string)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "test-token") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"message":"unauthorized"}`))
			return
		}
		switch r.URL.Path {
		case "/api/user/checkin":
			challengeID := r.URL.Query().Get("pow_challenge")
			nonce := r.URL.Query().Get("pow_nonce")
			if challengeID == "" || nonce == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"success":false,"message":"pow challenge required"}`))
				return
			}
			digest := sha256.Sum256([]byte(prefix + nonce))
			if !runAnyTimeDigestMatches(digest, difficulty) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"success":false,"message":"invalid pow"}`))
				return
			}
			*submittedChallenge = challengeID
			*submittedNonce = nonce
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"checkin_date":"2026-10-21","quota_awarded":12500000}}`))
		case "/api/user/pow/challenge":
			if r.URL.Query().Get("action") != "checkin" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"success":false,"message":"invalid action"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"success":true,"data":{"challenge_id":"challenge-1","prefix":%q,"difficulty":%d,"expires_at":%d}}`,
				prefix, difficulty, time.Now().Add(time.Minute).Unix(),
			)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, submittedChallenge, submittedNonce
}

func TestCheckinNewAPIPoSolvesAndSubmits(t *testing.T) {
	prefix := "checkin:1792565712:6573:57c20d569dc58d08:"
	server, submittedChallenge, submittedNonce := runAnyTimeTestServer(t, 10, prefix)

	result, accessToken, err := checkinRunAnyTime(context.Background(), &model.Site{
		Platform: model.SitePlatformRunAnyTime,
		BaseURL:  server.URL,
	}, &model.SiteAccount{
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "test-token",
	})
	if err != nil {
		t.Fatalf("checkinRunAnyTime returned error: %v", err)
	}
	if accessToken != "test-token" {
		t.Fatalf("expected access token to be preserved, got %q", accessToken)
	}
	if result.Status != model.SiteExecutionStatusSuccess {
		t.Fatalf("expected success, got %+v", result)
	}
	if !strings.Contains(result.Reward, "12500000") {
		t.Fatalf("expected quota_awarded reward, got %q", result.Reward)
	}
	if *submittedChallenge != "challenge-1" {
		t.Fatalf("expected challenge id to be submitted, got %q", *submittedChallenge)
	}
	if len(*submittedNonce) != runAnyTimeNonceDigits {
		t.Fatalf("expected 8-char nonce, got %q", *submittedNonce)
	}
}

func TestCheckinRunAnyTimeFallsBackWhenEndpointMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/checkin" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"reward":"100"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	result, _, err := checkinRunAnyTime(context.Background(), &model.Site{
		Platform: model.SitePlatformRunAnyTime,
		BaseURL:  server.URL,
	}, &model.SiteAccount{
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "test-token",
	})
	if err != nil {
		t.Fatalf("checkinRunAnyTime returned error: %v", err)
	}
	if result.Status != model.SiteExecutionStatusSuccess || result.Reward != "100" {
		t.Fatalf("expected plain checkin fallback result, got %+v", result)
	}
}

func TestSolveRunAnyTimeStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 257 bit 难度不可能在合理时间内解出，取消后必须立刻返回而不是继续空转。
	_, err := solveRunAnyTime(ctx, runAnyTimeChallenge{Prefix: "p:", Difficulty: 256})
	if err == nil {
		t.Fatalf("expected canceled context to abort solving")
	}
}

func TestCheckinRunAnyTimeReportsSubmitFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/pow/challenge":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"success":true,"data":{"challenge_id":"c1","prefix":"p:","difficulty":8,"expires_at":%d}}`,
				time.Now().Add(time.Minute).Unix(),
			)))
		case "/api/user/checkin":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"invalid pow"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, _, err := checkinRunAnyTime(context.Background(), &model.Site{
		Platform: model.SitePlatformRunAnyTime,
		BaseURL:  server.URL,
	}, &model.SiteAccount{
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "test-token",
	})
	if err != nil {
		t.Fatalf("checkinRunAnyTime should surface failures as result, got error: %v", err)
	}
	if result.Status != model.SiteExecutionStatusFailed || !strings.Contains(result.Message, "invalid pow") {
		t.Fatalf("expected submit failure result, got %+v", result)
	}
}

func TestRunAnyTimePlatformDoesNotAffectNewAPI(t *testing.T) {
	// 通用 new-api 平台不应被新平台牵连：签到分支仍走原先的内联实现。
	if model.SitePlatformNewAPI == model.SitePlatformRunAnyTime {
		t.Fatalf("runanytime must be a distinct platform")
	}
	if err := model.SitePlatformRunAnyTime.Validate(); err != nil {
		t.Fatalf("runanytime should be a valid platform: %v", err)
	}
}

func TestCheckinRunAnyTimeKeepsSkippedWhenNoCheckinEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	result, _, err := checkinRunAnyTime(context.Background(), &model.Site{
		Platform: model.SitePlatformRunAnyTime,
		BaseURL:  server.URL,
	}, &model.SiteAccount{
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "test-token",
	})
	if err != nil {
		t.Fatalf("checkinRunAnyTime returned error: %v", err)
	}
	if result.Status != model.SiteExecutionStatusSkipped {
		t.Fatalf("expected skipped status, got %+v", result)
	}
}
