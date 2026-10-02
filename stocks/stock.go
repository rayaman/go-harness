// fetch_10k.go
//
// Downloads SEC 10-K filings for a given company from EDGAR.
//
// Usage:
//
//	go run fetch_10k.go -ticker AAPL -n 5 -out ./filings
//	go run fetch_10k.go -cik 0000320193 -n 3 -out ./filings
//
// Notes:
//   - SEC requires a descriptive User-Agent with contact info on every request.
//     Set it via -useragent, or edit the default below.
//   - SEC EDGAR rate limit is ~10 requests/second; this script sleeps briefly
//     between requests to stay well under that.
package main

// const (
// 	tickerMapURL      = "https://www.sec.gov/files/company_tickers.json"
// 	submissionsURLFmt = "https://data.sec.gov/submissions/CIK%s.json"
// 	archivesBase      = "https://www.sec.gov/Archives/edgar/data"
// )

// // company_tickers.json is a map of arbitrary keys -> {cik_str, ticker, title}
// type tickerEntry struct {
// 	CIKStr int    `json:"cik_str"`
// 	Ticker string `json:"ticker"`
// 	Title  string `json:"title"`
// }

// // Subset of the SEC submissions API response we care about.
// type submissionsResp struct {
// 	Name    string `json:"name"`
// 	CIK     string `json:"cik"`
// 	Filings struct {
// 		Recent struct {
// 			Form            []string `json:"form"`
// 			FilingDate      []string `json:"filingDate"`
// 			AccessionNumber []string `json:"accessionNumber"`
// 			PrimaryDocument []string `json:"primaryDocument"`
// 			ReportDate      []string `json:"reportDate"`
// 		} `json:"recent"`
// 	} `json:"filings"`
// }

// func main() {
// 	ticker := flag.String("ticker", "", "Company ticker symbol, e.g. AAPL")
// 	cik := flag.String("cik", "", "Company CIK (with or without leading zeros)")
// 	n := flag.Int("n", 5, "Number of most recent 10-K filings to download")
// 	outDir := flag.String("out", "./filings", "Output directory")
// 	userAgent := flag.String("useragent", "MyCompany Research contact@example.com", "User-Agent header required by SEC (include contact info)")
// 	flag.Parse()

// 	if *ticker == "" && *cik == "" {
// 		fmt.Println("Error: must supply -ticker or -cik")
// 		flag.Usage()
// 		os.Exit(1)
// 	}

// 	client := &http.Client{Timeout: 30 * time.Second}

// 	resolvedCIK := *cik
// 	if resolvedCIK == "" {
// 		var err error
// 		resolvedCIK, err = lookupCIK(client, *userAgent, *ticker)
// 		if err != nil {
// 			fmt.Printf("Error resolving ticker %q: %v\n", *ticker, err)
// 			os.Exit(1)
// 		}
// 	}
// 	// SEC submissions API wants a 10-digit zero-padded CIK.
// 	paddedCIK := fmt.Sprintf("%010s", strings.TrimLeft(resolvedCIK, "0"))

// 	subs, err := fetchSubmissions(client, *userAgent, paddedCIK)
// 	if err != nil {
// 		fmt.Printf("Error fetching submissions: %v\n", err)
// 		os.Exit(1)
// 	}

// 	fmt.Printf("Company: %s (CIK %s)\n", subs.Name, subs.CIK)

// 	type filingInfo struct {
// 		date       string
// 		reportDate string
// 		accession  string
// 		doc        string
// 	}
// 	var tenKs []filingInfo
// 	recent := subs.Filings.Recent
// 	for i, form := range recent.Form {
// 		if form == "10-K" {
// 			tenKs = append(tenKs, filingInfo{
// 				date:       recent.FilingDate[i],
// 				reportDate: recent.ReportDate[i],
// 				accession:  recent.AccessionNumber[i],
// 				doc:        recent.PrimaryDocument[i],
// 			})
// 		}
// 		if len(tenKs) >= *n {
// 			break
// 		}
// 	}

// 	if len(tenKs) == 0 {
// 		fmt.Println("No 10-K filings found in the 'recent' submissions window.")
// 		fmt.Println("(Older filings live under subs.Filings.Files -- not handled by this script.)")
// 		return
// 	}

// 	if err := os.MkdirAll(*outDir, 0o755); err != nil {
// 		fmt.Printf("Error creating output dir: %v\n", err)
// 		os.Exit(1)
// 	}

// 	numericCIK := strings.TrimLeft(paddedCIK, "0")
// 	for _, f := range tenKs {
// 		accessionNoDashes := strings.ReplaceAll(f.accession, "-", "")
// 		docURL := fmt.Sprintf("%s/%s/%s/%s", archivesBase, numericCIK, accessionNoDashes, f.doc)

// 		fname := fmt.Sprintf("%s_10-K_%s_%s", subs.CIK, f.reportDate, filepath.Base(f.doc))
// 		fname = strings.ReplaceAll(fname, "/", "-")
// 		outPath := filepath.Join(*outDir, fname)

// 		fmt.Printf("Downloading 10-K filed %s (report date %s)\n  %s\n", f.date, f.reportDate, docURL)
// 		if err := downloadFile(client, *userAgent, docURL, outPath); err != nil {
// 			fmt.Printf("  Error: %v\n", err)
// 			continue
// 		}
// 		fmt.Printf("  Saved to %s\n", outPath)

// 		time.Sleep(200 * time.Millisecond) // stay well under SEC's rate limit
// 	}
// }

// func lookupCIK(client *http.Client, ua, ticker string) (string, error) {
// 	req, _ := http.NewRequest("GET", tickerMapURL, nil)
// 	req.Header.Set("User-Agent", ua)
// 	resp, err := client.Do(req)
// 	if err != nil {
// 		return "", err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode != http.StatusOK {
// 		return "", fmt.Errorf("unexpected status %d fetching ticker map", resp.StatusCode)
// 	}

// 	var m map[string]tickerEntry
// 	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
// 		return "", err
// 	}

// 	target := strings.ToUpper(ticker)
// 	for _, entry := range m {
// 		if strings.ToUpper(entry.Ticker) == target {
// 			return fmt.Sprintf("%d", entry.CIKStr), nil
// 		}
// 	}
// 	return "", fmt.Errorf("ticker %q not found", ticker)
// }

// func fetchSubmissions(client *http.Client, ua, paddedCIK string) (*submissionsResp, error) {
// 	url := fmt.Sprintf(submissionsURLFmt, paddedCIK)
// 	req, _ := http.NewRequest("GET", url, nil)
// 	req.Header.Set("User-Agent", ua)
// 	resp, err := client.Do(req)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode != http.StatusOK {
// 		return nil, fmt.Errorf("unexpected status %d fetching submissions (%s)", resp.StatusCode, url)
// 	}

// 	var out submissionsResp
// 	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
// 		return nil, err
// 	}
// 	return &out, nil
// }

// func downloadFile(client *http.Client, ua, url, outPath string) error {
// 	req, _ := http.NewRequest("GET", url, nil)
// 	req.Header.Set("User-Agent", ua)
// 	resp, err := client.Do(req)
// 	if err != nil {
// 		return err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode != http.StatusOK {
// 		return fmt.Errorf("unexpected status %d", resp.StatusCode)
// 	}

// 	out, err := os.Create(outPath)
// 	if err != nil {
// 		return err
// 	}
// 	defer out.Close()

// 	_, err = io.Copy(out, resp.Body)
// 	return err
// }

// parse_10k.go
//
// Parses a SEC 10-K filing (the primary inline-XBRL .htm document, the same
// file you get from EDGAR/data.sec.gov) into a structured Go type: filer
// metadata (from the standardized "dei" XBRL tags every 10-K includes) plus
// the body split into its numbered Items (Item 1, Item 1A, Item 7, etc).
//
// Usage as a library:
//
//	filing, err := ParseTenK("aapl-20210925.htm")
//	if err != nil { ... }
//	fmt.Println(filing.CompanyName, filing.FiscalYear)
//	for _, s := range filing.Sections {
//		fmt.Println(s.ItemNumber, s.Title, len(s.Content))
//	}
//
// Run directly for a quick CLI summary:
//
//	go run parse_10k.go aapl-20210925.htm

import (
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
)

// TenKFiling holds the structured result of parsing a 10-K filing document.
type TenKFiling struct {
	CompanyName       string    // dei:EntityRegistrantName
	CIK               string    // dei:EntityCentralIndexKey
	TradingSymbol     string    // dei:TradingSymbol
	ExchangeName      string    // dei:SecurityExchangeName
	DocumentType      string    // dei:DocumentType (should be "10-K")
	FiscalYear        string    // dei:DocumentFiscalYearFocus
	FiscalPeriod      string    // dei:DocumentFiscalPeriodFocus (usually "FY")
	PeriodEndDate     string    // dei:DocumentPeriodEndDate
	SharesOutstanding string    // dei:EntityCommonStockSharesOutstanding
	StateOfIncorp     string    // dei:EntityIncorporationStateCountryCode
	Sections          []Section // Item 1, Item 1A, Item 7, ... in document order
}

// Section is one numbered Item of the 10-K (e.g. "1A" / "Risk Factors").
type Section struct {
	ItemNumber string // "1", "1A", "7A", "9C", ...
	Title      string // "Risk Factors"
	Content    string // plain-text body of the section
}

// --- dei (Document Entity Information) tag extraction ------------------
//
// Every inline-XBRL 10-K carries these tags in a standardized, machine
// readable form regardless of filer/filing-agent, so this part of the
// parser generalizes across companies. Pattern:
//   <ix:nonNumeric ... name="dei:FieldName" ...>VALUE</ix:nonNumeric>

var deiTagRe = func(field string) *regexp.Regexp {
	return regexp.MustCompile(`(?is)<ix:nonNumeric[^>]*name="dei:` + field + `"[^>]*>(.*?)</ix:nonNumeric>`)
}

func extractDEI(raw, field string) string {
	m := deiTagRe(field).FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	return cleanText(m[1])
}

// cleanText strips any nested tags from a matched value, unescapes HTML
// entities, normalizes non-breaking spaces, and trims whitespace.
func cleanText(s string) string {
	s = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.TrimSpace(s)
}

// --- HTML -> plain text (block-aware) -----------------------------------

var (
	scriptStyleRe  = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blockBreakRe   = regexp.MustCompile(`(?i)</(div|p|tr|table|br|h[1-6])\s*>`)
	tagRe          = regexp.MustCompile(`(?s)<[^>]+>`)
	multiSpaceRe   = regexp.MustCompile(`[ \t]+`)
	multiNewlineRe = regexp.MustCompile(`\n{2,}`)
)

// htmlToText converts the raw filing HTML into normalized plain text,
// inserting a newline at block-level element boundaries so that section
// headers land on their own line (needed for the Item-splitting regex).
func htmlToText(raw string) string {
	t := scriptStyleRe.ReplaceAllString(raw, " ")
	t = blockBreakRe.ReplaceAllString(t, "\n")
	t = tagRe.ReplaceAllString(t, "")
	t = html.UnescapeString(t)
	t = strings.ReplaceAll(t, "\u00a0", " ") // NBSP -> normal space
	t = multiSpaceRe.ReplaceAllString(t, " ")
	t = multiNewlineRe.ReplaceAllString(t, "\n")
	return t
}

// --- Item section splitting ---------------------------------------------

// Matches lines like "Item 1A. Risk Factors" or "Item 6. [Reserved]".
// 10-K filings restate the full Item list in the table of contents before
// the real section headers appear in the body, so this pattern matches
// both; we keep only the LAST match per item number, since the body
// (which comes after the TOC) always contains the final occurrence of
// each header.
var itemHeaderRe = regexp.MustCompile(
	`(?m)\bItem\s+(\d{1,2}[A-C]?)\.\s*([A-Z\[][A-Za-z0-9,\x{2019}'&\-/\[\] ]{2,160}?)\s*\n`,
)

type itemMatch struct {
	start, end        int
	itemNumber, title string
}

func findItemHeaders(text string) []itemMatch {
	locs := itemHeaderRe.FindAllStringSubmatchIndex(text, -1)
	lastByItem := map[string]itemMatch{}
	order := []string{}
	for _, loc := range locs {
		full := text[loc[0]:loc[1]]
		itemNum := text[loc[2]:loc[3]]
		title := strings.TrimSpace(text[loc[4]:loc[5]])
		m := itemMatch{start: loc[0], end: loc[0] + len(full), itemNumber: itemNum, title: title}
		if _, seen := lastByItem[itemNum]; !seen {
			order = append(order, itemNum)
		}
		lastByItem[itemNum] = m // keep overwriting -> last occurrence wins
	}
	result := make([]itemMatch, 0, len(order))
	for _, k := range order {
		result = append(result, lastByItem[k])
	}
	// sort by position (order of first-seen key isn't necessarily position
	// order once we've kept the *last* match for each key)
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].start < result[j-1].start; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

// splitSections slices the plain-text body between consecutive Item
// headers to build the section list.
func splitSections(text string) []Section {
	headers := findItemHeaders(text)
	sections := make([]Section, 0, len(headers))
	for i, h := range headers {
		end := len(text)
		if i+1 < len(headers) {
			end = headers[i+1].start
		}
		content := strings.TrimSpace(text[h.end:end])
		sections = append(sections, Section{
			ItemNumber: h.itemNumber,
			Title:      h.title,
			Content:    content,
		})
	}
	return sections
}

// ParseTenK reads a 10-K primary document (.htm, inline XBRL) from disk and
// returns a structured TenKFiling.
func ParseTenK(path string) (*TenKFiling, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	return ParseTenKBytes(raw)
}

// ParseTenKBytes parses already-loaded filing HTML bytes.
func ParseTenKBytes(raw []byte) (*TenKFiling, error) {
	s := string(raw)

	filing := &TenKFiling{
		CompanyName:       extractDEI(s, "EntityRegistrantName"),
		CIK:               extractDEI(s, "EntityCentralIndexKey"),
		TradingSymbol:     extractDEI(s, "TradingSymbol"),
		ExchangeName:      extractDEI(s, "SecurityExchangeName"),
		DocumentType:      extractDEI(s, "DocumentType"),
		FiscalYear:        extractDEI(s, "DocumentFiscalYearFocus"),
		FiscalPeriod:      extractDEI(s, "DocumentFiscalPeriodFocus"),
		PeriodEndDate:     extractDEI(s, "DocumentPeriodEndDate"),
		SharesOutstanding: extractDEI(s, "EntityCommonStockSharesOutstanding"),
		StateOfIncorp:     extractDEI(s, "EntityIncorporationStateCountryCode"),
	}

	text := htmlToText(s)
	filing.Sections = splitSections(text)

	return filing, nil
}

// --- CLI entry point (optional; comment out or remove if only using as a
// library import) --------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run parse_10k.go <path-to-10K.htm>")
		os.Exit(1)
	}

	filing, err := ParseTenK(os.Args[1])
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=== Filer info ===")
	fmt.Printf("Company:       %s\n", filing.CompanyName)
	fmt.Printf("CIK:           %s\n", filing.CIK)
	fmt.Printf("Ticker:        %s (%s)\n", filing.TradingSymbol, filing.ExchangeName)
	fmt.Printf("Document type: %s\n", filing.DocumentType)
	fmt.Printf("Fiscal year:   %s %s\n", filing.FiscalYear, filing.FiscalPeriod)
	fmt.Printf("Period end:    %s\n", filing.PeriodEndDate)
	fmt.Printf("Shares out.:   %s\n", filing.SharesOutstanding)
	fmt.Printf("Incorporated:  %s\n", filing.StateOfIncorp)

	fmt.Printf("\n=== Sections found: %d ===\n", len(filing.Sections))
	for _, sec := range filing.Sections {
		preview := sec.Content
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		fmt.Printf("Item %-4s %-70s (%6d chars)  %q\n", sec.ItemNumber, sec.Title, len(sec.Content), preview)
	}
}
