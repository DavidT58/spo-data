package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal Telegram Bot API client that only sends messages. It
// intentionally avoids a third-party dependency, mirroring the lightweight
// hand-rolled HTTP clients elsewhere in this repo (see internal/lbank).
type Client struct {
	Token      string
	ChatID     string
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient initializes a Telegram client for a single bot token + chat ID.
func NewClient(token, chatID string) *Client {
	return &Client{
		Token:   token,
		ChatID:  chatID,
		BaseURL: "https://api.telegram.org",
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type sendResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// Send delivers a Markdown-formatted message to the configured chat. It returns
// an error on transport or API failure; callers should log and continue rather
// than crash the daemon.
func (c *Client) Send(text string) error {
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", c.BaseURL, c.Token)

	form := url.Values{}
	form.Set("chat_id", c.ChatID)
	form.Set("text", text)
	form.Set("parse_mode", "Markdown")
	// Pool/operator names can contain underscores etc.; never let formatting
	// errors swallow an alert.
	form.Set("disable_web_page_preview", "true")

	resp, err := c.HTTPClient.PostForm(endpoint, form)
	if err != nil {
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	var parsed sendResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("telegram: failed to decode response (status %d): %w", resp.StatusCode, err)
	}
	if !parsed.OK {
		return fmt.Errorf("telegram API error %d: %s", parsed.ErrorCode, strings.TrimSpace(parsed.Description))
	}
	return nil
}
