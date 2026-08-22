package bdo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"
)

const (
	BaseURL = "https://api.blackdesertmarket.com"
)

// Region represents supported BDO server regions.
type Region string

const (
	RegionNA Region = "na"
	RegionEU Region = "eu"
	RegionSA Region = "sa"
	RegionKR Region = "kr"
)

// Client handles requests to the Black Desert Market API.
type Client struct {
	baseURL    string
	region     Region
	language   string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// Option configures the Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// WithRegion sets the target market region (default: "na").
func WithRegion(region Region) Option {
	return func(c *Client) {
		c.region = region
	}
}

// WithLanguage sets the response language code (default: "en").
func WithLanguage(lang string) Option {
	return func(c *Client) {
		c.language = lang
	}
}

// WithRateLimit sets a custom rate limit.
// r: requests per second allowed (e.g. rate.Every(200 * time.Millisecond) or rate.Limit(5))
// b: burst capacity (maximum tokens buffered)
func WithRateLimit(r rate.Limit, b int) Option {
	return func(c *Client) {
		c.limiter = rate.NewLimiter(r, b)
	}
}

// NewClient creates a new BDO Market API client.
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL:  BaseURL,
		region:   RegionNA,
		language: "en",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		// Default rate limit: 5 requests per second, burst of 5
		limiter: rate.NewLimiter(rate.Limit(5), 5),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// API Models

type MarketItemOverview struct {
	Code string         `json:"code"`
	Data []OverviewData `json:"data"`
}

type MarketQueueOverview struct {
	Code string      `json:"code"`
	Data []QueueData `json:"data"`
}

type QueueData struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Count        int64     `json:"count"`
	Grade        int       `json:"grade"`
	BasePrice    int64     `json:"basePrice"`
	MainCategory int       `json:"mainCategory"`
	SubCategory  int       `json:"subCategory"`
	Enhancement  int       `json:"enhancement"`
	EndTime      time.Time `json:"endTime"`
}

type SearchItemOverview struct {
	Code string       `json:"code"`
	Data []SearchData `json:"data"`
}

type SearchData struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Count     int64  `json:"count"`
	Grade     int    `json:"grade"`
	BasePrice int64  `json:"basePrice"`
}

type OverviewData struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Grade        int    `json:"grade"`
	Enhancement  int    `json:"enhancement"`
	BasePrice    int64  `json:"basePrice"`
	Count        int64  `json:"count"`
	TradeCount   int64  `json:"tradeCount"`
	SubCategory  int    `json:"subCategory"`
	MainCategory int    `json:"mainCategory"`
}

type MarketItemDetail struct {
	Code string     `json:"code"`
	Data MarketData `json:"data"`
}

type MarketData struct {
	Availability []ItemAvailability `json:"availability"`
	History      []History          `json:"history"`
	BasePrice    int64              `json:"basePrice"`
	SellCount    int                `json:"sellCount"`
}

type History struct {
	Date     time.Time `json:"date"`
	OnePrice int64     `json:"onePrice"`
}

type ItemAvailability struct {
	SellCount int   `json:"sellCount"`
	BuyCount  int   `json:"buyCount"`
	OnePrice  int64 `json:"onePrice"`
}

type PricePoint struct {
	Timestamp int64 `json:"timestamp"`
	Price     int64 `json:"price"`
}

type OrderBook struct {
	Price int64 `json:"price"`
	Count int64 `json:"count"`
}

type ResponseEnvelope struct {
	Code       string               `json:"code,omitempty"`
	ResultCode int                  `json:"resultCode,omitempty"`
	ResultMsg  string               `json:"resultMsg,omitempty"`
	Data       []MarketItemOverview `json:"data,omitempty"`
	Items      []MarketItemOverview `json:"items,omitempty"`
	Result     []MarketItemOverview `json:"result,omitempty"`
}

// Helper Methods & API Calls

// send executes the HTTP request after acquiring a rate-limiting token.
func (c *Client) send(ctx context.Context, endpoint string, result interface{}) error {
	body, err := c.requestBytes(ctx, endpoint)
	if err != nil {
		return err
	}

	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("decoding response into %T: %w (raw body: %s)", result, err, string(body))
		}
	}

	return nil
}

// GetHotList retrieves items currently on the hot market list.
func (c *Client) GetHotList(ctx context.Context) (*MarketItemOverview, error) {
	// endpoint := fmt.Sprintf("/list/hot?region=%s&lang=%s", c.region, c.language)
	return nil, fmt.Errorf("Response from external market did contain invalid data")
}

// GetQueueList retrieves items in the pre-order queue list.
func (c *Client) GetQueueList(ctx context.Context) (*MarketQueueOverview, error) {
	endpoint := fmt.Sprintf("/list/queue?region=%s&lang=%s", c.region, c.language)
	var item *MarketQueueOverview
	if err := c.send(ctx, endpoint, &item); err == nil && item.Code == "SUCCESS" {
		return item, nil
	}
	return nil, fmt.Errorf("unable to get queuelist for item")
}

// GetCategoryItems retrieves items in a given category and subcategory.
func (c *Client) GetCategoryItems(ctx context.Context, mainCategory, subCategory int) (*SearchItemOverview, error) {
	endpoint := fmt.Sprintf("/list/%d/%d?region=%s&lang=%s", mainCategory, subCategory, c.region, c.language)
	var item *SearchItemOverview
	if err := c.send(ctx, endpoint, &item); err == nil && item.Code == "SUCCESS" {
		return item, nil
	}
	return nil, fmt.Errorf("unable to search for item %d/%d", mainCategory, subCategory)
}

// Search queries the market by string name.
func (c *Client) Search(ctx context.Context, query string) (*SearchItemOverview, error) {
	endpoint := fmt.Sprintf("/search/%s?region=%s&lang=%s", url.PathEscape(query), c.region, c.language)
	var item *SearchItemOverview
	if err := c.send(ctx, endpoint, &item); err == nil && item.Code == "SUCCESS" {
		return item, nil
	}
	return nil, fmt.Errorf("unable to search for item %v", query)
}

// GetItemOverview retrieves summary data for a single item ID.
func (c *Client) GetItemOverview(ctx context.Context, itemID int) (*MarketItemOverview, error) {
	endpoint := fmt.Sprintf("/item/%d?region=%s&lang=%s", itemID, c.region, c.language)

	// 1. Try decoding as a single object
	var item MarketItemOverview
	if err := c.send(ctx, endpoint, &item); err == nil && item.Code == "SUCCESS" {
		return &item, nil
	}

	return nil, fmt.Errorf("unable to parse item overview for ID %d", itemID)
}

func normalizeStock(items []MarketItemOverview) []MarketItemOverview {
	return items
}

func (c *Client) requestBytes(ctx context.Context, endpoint string) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter wait failed: %w", err)
	}

	reqURL := fmt.Sprintf("%s%s", c.baseURL, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "BDO-Market-Client-Go/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api error (status %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetItemDetail fetches detailed market data for an item ID and enhancement level.
func (c *Client) GetItemDetail(ctx context.Context, itemID, enhancement int) (*MarketItemDetail, error) {
	endpoint := fmt.Sprintf("/item/%d/%d?region=%s&lang=%s", itemID, enhancement, c.region, c.language)

	// 1. Try direct object unmarshaling
	var detail MarketItemDetail
	if err := c.send(ctx, endpoint, &detail); err == nil {
		return &detail, nil
	}
	if detail.Code == "SUCCESS" {
		return &detail, nil
	}

	return nil, fmt.Errorf("unable to parse item detail for ID %d / enhancement %d", itemID, enhancement)
}

func isPopulated(d *MarketItemDetail) bool {
	return len(d.Data.Availability) > 0
}

// GetItemIconURL returns the direct image URL for an item.
func (c *Client) GetItemIconURL(itemID int) string {
	return fmt.Sprintf("%s/item/%d/icon", c.baseURL, itemID)
}

// --- High-Level Convenience Helpers ---
type BatchResult struct {
	Items  map[int]*MarketItemOverview
	Errors map[int]error
}

// BatchGetItems fetches detailed info for multiple item IDs while adhering to rate limits.
func (c *Client) BatchGetItems(ctx context.Context, itemIDs []int) BatchResult {
	res := BatchResult{
		Items:  make(map[int]*MarketItemOverview),
		Errors: make(map[int]error),
	}

	for _, id := range itemIDs {
		item, err := c.GetItemOverview(ctx, id)
		if err != nil {
			res.Errors[id] = err
			continue
		}
		res.Items[id] = item
	}

	return res
}
