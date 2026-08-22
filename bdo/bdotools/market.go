package bdotools

import (
	"ai-harness/bdo"
	"ai-harness/harness"
	"context"
	"encoding/json"
	"fmt"

	"golang.org/x/time/rate"
)

// --- 1. Hot List Tool ---

type GetHotListTool struct {
	client *bdo.Client
}

func NewGetHotListTool(c *bdo.Client) GetHotListTool {
	return GetHotListTool{client: c}
}

func (t GetHotListTool) Name() string { return "bdo_get_hot_list" }
func (t GetHotListTool) Description() string {
	return "Get items currently on the BDO market hot list, optionally filtered by minimum stock."
}

func (t GetHotListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"min_stock": map[string]any{
				"type":        "integer",
				"description": "Optional minimum stock count to filter high-demand hot items.",
			},
		},
	}
}

func (t GetHotListTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		MinStock *int64 `json:"min_stock"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &input)
	}

	var items *bdo.MarketItemOverview
	var err error
	items, err = t.client.GetHotList(context.Background())
	if err != nil {
		return "", fmt.Errorf("failed to get hot list: %w", err)
	}

	res, err := json.Marshal(items)
	return string(res), err
}

// --- 2. Pre-Order Queue Tool ---

type GetQueueListTool struct {
	client *bdo.Client
}

func NewGetQueueListTool(c *bdo.Client) GetQueueListTool {
	return GetQueueListTool{client: c}
}

func (t GetQueueListTool) Name() string { return "bdo_get_queue_list" }
func (t GetQueueListTool) Description() string {
	return "Retrieve items currently in the pre-order market queue list."
}

func (t GetQueueListTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t GetQueueListTool) Execute(_ json.RawMessage) (string, error) {
	items, err := t.client.GetQueueList(context.Background())
	if err != nil {
		return "", fmt.Errorf("failed to get queue list: %w", err)
	}

	res, err := json.Marshal(items)
	return string(res), err
}

// --- 3. Category Items Tool ---

type GetCategoryItemsTool struct {
	client *bdo.Client
}

func NewGetCategoryItemsTool(c *bdo.Client) GetCategoryItemsTool {
	return GetCategoryItemsTool{client: c}
}

func (t GetCategoryItemsTool) Name() string { return "bdo_get_category_items" }
func (t GetCategoryItemsTool) Description() string {
	return "Retrieve market items under a specific main category and subcategory."
}

func (t GetCategoryItemsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"main_category": map[string]any{
				"type":        "integer",
				"description": "Main category ID (e.g. 1 for Main Weapon, 20 for Consumables)",
			},
			"sub_category": map[string]any{
				"type":        "integer",
				"description": "Subcategory ID within the main category",
			},
		},
		"required": []string{"main_category", "sub_category"},
	}
}

func (t GetCategoryItemsTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		MainCategory int `json:"main_category"`
		SubCategory  int `json:"sub_category"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	items, err := t.client.GetCategoryItems(context.Background(), input.MainCategory, input.SubCategory)
	if err != nil {
		return "", fmt.Errorf("failed to get category items: %w", err)
	}

	res, err := json.Marshal(items)
	return string(res), err
}

// --- 4. Market Search Tool ---

type SearchMarketTool struct {
	client *bdo.Client
}

func NewSearchMarketTool(c *bdo.Client) SearchMarketTool {
	return SearchMarketTool{client: c}
}

func (t SearchMarketTool) Name() string { return "bdo_search_market" }
func (t SearchMarketTool) Description() string {
	return "Search the Central Market for items matching a string query."
}

func (t SearchMarketTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search term or item name (e.g., 'Black Stone', 'Ogre Ring')",
			},
		},
		"required": []string{"query"},
	}
}

func (t SearchMarketTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	items, err := t.client.Search(context.Background(), input.Query)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	res, err := json.Marshal(items)
	return string(res), err
}

// --- 5. Single Item Overview Tool ---

type GetItemOverviewTool struct {
	client *bdo.Client
}

func NewGetItemOverviewTool(c *bdo.Client) GetItemOverviewTool {
	return GetItemOverviewTool{client: c}
}

func (t GetItemOverviewTool) Name() string { return "bdo_get_item_overview" }
func (t GetItemOverviewTool) Description() string {
	return "Get summary overview data for a single item by its numerical ID."
}

func (t GetItemOverviewTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item_id": map[string]any{
				"type":        "integer",
				"description": "Numerical item ID",
			},
		},
		"required": []string{"item_id"},
	}
}

func (t GetItemOverviewTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		ItemID int `json:"item_id"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	item, err := t.client.GetItemOverview(context.Background(), input.ItemID)
	if err != nil {
		return "", fmt.Errorf("failed to get item overview: %w", err)
	}

	res, err := json.Marshal(item)
	return string(res), err
}

// --- 6. Item Detail Tool ---

type GetItemDetailTool struct {
	client *bdo.Client
}

func NewGetItemDetailTool(c *bdo.Client) GetItemDetailTool {
	return GetItemDetailTool{client: c}
}

func (t GetItemDetailTool) Name() string { return "bdo_get_item_detail" }
func (t GetItemDetailTool) Description() string {
	return "Get detailed market data (bidding order book, price history) for a specific item ID and enhancement level."
}

func (t GetItemDetailTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item_id": map[string]any{
				"type":        "integer",
				"description": "Numerical item ID",
			},
			"enhancement": map[string]any{
				"type":        "integer",
				"description": "Enhancement level (0-20 or 0-5 for accessories)",
			},
		},
		"required": []string{"item_id", "enhancement"},
	}
}

func (t GetItemDetailTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		ItemID      int `json:"item_id"`
		Enhancement int `json:"enhancement"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	detail, err := t.client.GetItemDetail(context.Background(), input.ItemID, input.Enhancement)
	if err != nil {
		return "", fmt.Errorf("failed to get item detail: %w", err)
	}

	res, err := json.Marshal(detail)
	return string(res), err
}

// --- 7. Batch Items Overview Tool ---

type BatchGetItemsTool struct {
	client *bdo.Client
}

func NewBatchGetItemsTool(c *bdo.Client) BatchGetItemsTool {
	return BatchGetItemsTool{client: c}
}

func (t BatchGetItemsTool) Name() string { return "bdo_batch_get_items" }
func (t BatchGetItemsTool) Description() string {
	return "Fetch summary overview details for multiple item IDs simultaneously while managing rate limits."
}

func (t BatchGetItemsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item_ids": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "integer",
				},
				"description": "Array of item IDs to fetch",
			},
		},
		"required": []string{"item_ids"},
	}
}

func (t BatchGetItemsTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		ItemIDs []int `json:"item_ids"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	result := t.client.BatchGetItems(context.Background(), input.ItemIDs)

	res, err := json.Marshal(result)
	return string(res), err
}

// GetCategoryNamesTool
type GetCategoryNamesTool struct {
	client *bdo.Client
}

func NewGetCategoryNamesTool(c *bdo.Client) GetCategoryNamesTool {
	return GetCategoryNamesTool{client: c}
}

func (t GetCategoryNamesTool) Name() string { return "bdo_get_category_names" }
func (t GetCategoryNamesTool) Description() string {
	return "Get the main category name and sub category name for a given mainCategory ID and subCategory ID."
}

func (t GetCategoryNamesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mainCategory": map[string]any{
				"type":        "integer",
				"description": "The main category ID (e.g., 1 for Main Weapon, 15 for Armor)",
			},
			"subCategory": map[string]any{
				"type":        "integer",
				"description": "The sub category ID within the main category",
			},
		},
		"required": []string{"mainCategory", "subCategory"},
	}
}

func (t GetCategoryNamesTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		MainCategory int `json:"mainCategory"`
		SubCategory  int `json:"subCategory"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	mainName, subName, err := bdo.GetCategoryNames(input.MainCategory, input.SubCategory)
	if err != nil {
		return "", fmt.Errorf("get category names failed: %w", err)
	}

	res, err := json.Marshal(map[string]string{
		"mainCategoryName": mainName,
		"subCategoryName":  subName,
	})
	return string(res), err
}

// GetMainCategoryNameTool
type GetMainCategoryNameTool struct {
	client *bdo.Client
}

func NewGetMainCategoryNameTool(c *bdo.Client) GetMainCategoryNameTool {
	return GetMainCategoryNameTool{client: c}
}

func (t GetMainCategoryNameTool) Name() string { return "bdo_get_main_category_name" }
func (t GetMainCategoryNameTool) Description() string {
	return "Get the main category name for a given main category ID."
}

func (t GetMainCategoryNameTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mainCategory": map[string]any{
				"type":        "integer",
				"description": "The main category ID (e.g., 1 for Main Weapon, 15 for Armor)",
			},
		},
		"required": []string{"mainCategory"},
	}
}

func (t GetMainCategoryNameTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		MainCategory int `json:"mainCategory"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	name, err := bdo.GetMainCategoryName(input.MainCategory)
	if err != nil {
		return "", fmt.Errorf("get main category name failed: %w", err)
	}

	res, err := json.Marshal(map[string]string{
		"mainCategoryName": name,
	})
	return string(res), err
}

// GetCategoryIDsTool
type GetCategoryIDsTool struct {
	client *bdo.Client
}

func NewGetCategoryIDsTool(c *bdo.Client) GetCategoryIDsTool {
	return GetCategoryIDsTool{client: c}
}

func (t GetCategoryIDsTool) Name() string { return "bdo_get_category_ids" }
func (t GetCategoryIDsTool) Description() string {
	return "Get the main category ID and sub category ID for given main category name and sub category name."
}

func (t GetCategoryIDsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mainCategoryName": map[string]any{
				"type":        "string",
				"description": "The main category name (e.g., 'Main Weapon', 'Armor')",
			},
			"subCategoryName": map[string]any{
				"type":        "string",
				"description": "The sub category name (e.g., 'Longsword', 'Helmet')",
			},
		},
		"required": []string{"mainCategoryName", "subCategoryName"},
	}
}

func (t GetCategoryIDsTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		MainCategoryName string `json:"mainCategoryName"`
		SubCategoryName  string `json:"subCategoryName"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	mainID, subID, err := bdo.GetCategoryIDs(input.MainCategoryName, input.SubCategoryName)
	if err != nil {
		return "", fmt.Errorf("get category IDs failed: %w", err)
	}

	res, err := json.Marshal(map[string]int{
		"mainCategoryID": mainID,
		"subCategoryID":  subID,
	})
	return string(res), err
}

// GetMainCategoryIDTool
type GetMainCategoryIDTool struct {
	client *bdo.Client
}

func NewGetMainCategoryIDTool(c *bdo.Client) GetMainCategoryIDTool {
	return GetMainCategoryIDTool{client: c}
}

func (t GetMainCategoryIDTool) Name() string { return "bdo_get_main_category_id" }
func (t GetMainCategoryIDTool) Description() string {
	return "Get the main category ID for a given main category name."
}

func (t GetMainCategoryIDTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mainCategoryName": map[string]any{
				"type":        "string",
				"description": "The main category name (e.g., 'Main Weapon', 'Armor')",
			},
		},
		"required": []string{"mainCategoryName"},
	}
}

func (t GetMainCategoryIDTool) Execute(args json.RawMessage) (string, error) {
	var input struct {
		MainCategoryName string `json:"mainCategoryName"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	id, err := bdo.GetMainCategoryID(input.MainCategoryName)
	if err != nil {
		return "", fmt.Errorf("get main category ID failed: %w", err)
	}

	res, err := json.Marshal(map[string]int{
		"mainCategoryID": id,
	})
	return string(res), err
}

func RegisterMarketTools(r *harness.Registry) {
	c := bdo.NewClient(bdo.WithLanguage("en"),
		bdo.WithRegion(bdo.RegionNA),
		bdo.WithRateLimit(rate.Limit(3), 5))
	r.Register(GetHotListTool{client: c})
	r.Register(GetQueueListTool{client: c})
	r.Register(GetCategoryItemsTool{client: c})
	r.Register(SearchMarketTool{client: c})
	r.Register(GetItemOverviewTool{client: c})
	r.Register(GetItemDetailTool{client: c})
	r.Register(BatchGetItemsTool{client: c})
	r.Register(GetCategoryNamesTool{client: c})
	r.Register(GetMainCategoryNameTool{client: c})
	r.Register(GetCategoryIDsTool{client: c})
	r.Register(GetMainCategoryIDTool{client: c})
}
