package mexc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

type PriceResponse struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

// NewClient initializes a new MEXC API client.
func NewClient() *Client {
	return &Client{
		BaseURL: "https://api.mexc.com/api/v3",
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetPrice fetches the ticker price for a given symbol.
func (c *Client) GetPrice() (*PriceResponse, error) {
	url := fmt.Sprintf("%s/ticker/price?symbol=AP3XUSDT", c.BaseURL)

	fmt.Println(url)

	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var price PriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&price); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &price, nil
}
