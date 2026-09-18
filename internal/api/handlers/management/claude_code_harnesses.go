package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	claudeCodeHarnessUsagePath = "/v1/harness/usage"
	claudeCodeHarnessMaxBody   = 1 << 20
)

type claudeCodeHarnessSource struct {
	ID           string
	Name         string
	BaseURL      string
	APIKey       string
	ProviderName string
}

func claudeCodeHarnessID(provider config.OpenAICompatibility) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(provider.Name) + "\x00" + strings.TrimSpace(provider.BaseURL)))
	return hex.EncodeToString(digest[:8])
}

func (h *Handler) claudeCodeHarnessSources() []claudeCodeHarnessSource {
	h.mu.Lock()
	var snapshot *config.Config
	if h.cfg != nil {
		snapshot = h.cfg.CloneForRuntime()
	}
	h.mu.Unlock()
	if snapshot == nil {
		return nil
	}

	sources := make([]claudeCodeHarnessSource, 0)
	for _, provider := range snapshot.OpenAICompatibility {
		if provider.Disabled || provider.ClaudeCodeHarness == nil || len(provider.APIKeyEntries) == 0 {
			continue
		}
		apiKey := strings.TrimSpace(provider.APIKeyEntries[0].APIKey)
		baseURL := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		if apiKey == "" || baseURL == "" {
			continue
		}
		name := strings.TrimSpace(provider.ClaudeCodeHarness.DisplayName)
		if name == "" {
			name = strings.TrimSpace(provider.Name)
		}
		if name == "" {
			name = "Claude Code Harness"
		}
		sources = append(sources, claudeCodeHarnessSource{
			ID:           claudeCodeHarnessID(provider),
			Name:         name,
			BaseURL:      baseURL,
			APIKey:       apiKey,
			ProviderName: strings.TrimSpace(provider.Name),
		})
	}
	return sources
}

// ListClaudeCodeHarnesses exposes non-secret metadata for official Claude Code
// workers configured as OpenAI-compatible providers.
func (h *Handler) ListClaudeCodeHarnesses(c *gin.Context) {
	sources := h.claudeCodeHarnessSources()
	items := make([]gin.H, 0, len(sources))
	for _, source := range sources {
		items = append(items, gin.H{
			"id":            source.ID,
			"name":          source.Name,
			"provider_name": source.ProviderName,
		})
	}
	c.JSON(http.StatusOK, gin.H{"harnesses": items})
}

// FetchClaudeCodeHarnessQuota proxies the adapter's authenticated, normalized
// Claude Code /usage response without returning the adapter credential.
func (h *Handler) FetchClaudeCodeHarnessQuota(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	var selected *claudeCodeHarnessSource
	for _, source := range h.claudeCodeHarnessSources() {
		if source.ID == id {
			copySource := source
			selected = &copySource
			break
		}
	}
	if selected == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Claude Code harness not found"})
		return
	}

	base, errParse := url.Parse(selected.BaseURL)
	if errParse != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Claude Code harness has an invalid base URL"})
		return
	}
	base.Path = strings.TrimRight(base.Path, "/")
	if strings.HasSuffix(base.Path, "/v1") {
		base.Path += "/harness/usage"
	} else {
		base.Path += claudeCodeHarnessUsagePath
	}
	base.RawQuery = ""
	base.Fragment = ""

	ctx, cancel := context.WithTimeout(c.Request.Context(), 35*time.Second)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if errRequest != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to build Claude Code harness request"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+selected.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("Claude Code harness unavailable: %v", errDo)})
		return
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, claudeCodeHarnessMaxBody+1))
	if errRead != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read Claude Code harness quota"})
		return
	}
	if len(body) > claudeCodeHarnessMaxBody {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Claude Code harness quota response is too large"})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("Claude Code harness returned HTTP %d", resp.StatusCode)})
		return
	}
	var payload map[string]any
	if errJSON := json.Unmarshal(body, &payload); errJSON != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Claude Code harness returned invalid JSON"})
		return
	}
	if _, ok := payload["usage"].(map[string]any); !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Claude Code harness returned no usage data"})
		return
	}
	payload["id"] = selected.ID
	payload["name"] = selected.Name
	c.JSON(http.StatusOK, payload)
}
