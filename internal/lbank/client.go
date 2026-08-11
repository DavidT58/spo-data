package lbank

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
	// Symbol string  `json:"symbol"`
	// Price  float64 `json:"price,string"
	Result    bool `json:"result"`
	ErrorCode int  `json:"errorCode"`
	Data      []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	} `json:"data"`
}

// NewClient initializes a new LBANK API client.
func NewClient() *Client {
	return &Client{
		BaseURL: "https://api.lbkex.com/v2",
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetPrice fetches the ticker price for a given symbol.
func (c *Client) GetPrice(symbol string) (*PriceResponse, error) {
	url := fmt.Sprintf("%s/supplement/ticker/price.do?symbol=%s", c.BaseURL, symbol)

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
