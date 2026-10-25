package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Graph is a minimal Microsoft Graph client for the read-only calls sptree needs.
type Graph struct {
	base string // https://graph.microsoft.com/v1.0
	auth *Auth
	hc   *http.Client

	libMu    sync.Mutex
	libs     []Library
	libsTime time.Time
}

func NewGraph(base string, auth *Auth) *Graph {
	return &Graph{base: strings.TrimRight(base, "/"), auth: auth, hc: &http.Client{Timeout: 60 * time.Second}}
}

// GraphError is a non-2xx response from Graph.
type GraphError struct {
	Status  int
	Code    string
	Message string
}

func (e *GraphError) Error() string {
	return fmt.Sprintf("graph %d %s: %s", e.Status, e.Code, e.Message)
}

func (g *Graph) do(ctx context.Context, method, pathOrURL string, body any, out any) error {
	u := pathOrURL
	if !strings.HasPrefix(u, "http") {
		u = g.base + u
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	forced := false
	for attempt := 0; attempt < 5; attempt++ {
		tok, err := g.auth.AccessToken(ctx, forced)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.hc.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode == 401 && !forced:
			forced = true
			continue
		case resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 504:
			wait := 2 * time.Second << attempt
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(s) * time.Second
			}
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		case resp.StatusCode >= 300:
			var ge struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			_ = json.Unmarshal(data, &ge)
			return &GraphError{Status: resp.StatusCode, Code: ge.Error.Code, Message: ge.Error.Message}
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(data, out)
	}
	return &GraphError{Status: 429, Code: "throttled", Message: "Microsoft Graph kept throttling the request"}
}

func (g *Graph) get(ctx context.Context, p string, out any) error {
	return g.do(ctx, "GET", p, nil, out)
}

// ---- types returned to the web UI ----

type Library struct {
	DriveID string `json:"driveId"`
	Site    string `json:"site"`
	SiteURL string `json:"siteUrl"`
	Name    string `json:"name"`
	WebURL  string `json:"webUrl"`
	Kind    string `json:"kind"` // documentLibrary | business (OneDrive)
}

type Item struct {
	ID         string `json:"id"`
	DriveID    string `json:"driveId"`
	Name       string `json:"name"`
	WebURL     string `json:"webUrl"`
	Folder     bool   `json:"folder"`
	ChildCount int    `json:"childCount,omitempty"`
	Size       int64  `json:"size"`
	Modified   string `json:"modified,omitempty"`
	Path       string `json:"path,omitempty"` // human path of the parent (search results only)
	Site       string `json:"site,omitempty"`
}

type driveItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	WebURL   string `json:"webUrl"`
	Size     int64  `json:"size"`
	Modified string `json:"lastModifiedDateTime"`
	Folder   *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	Root            *struct{} `json:"root"`
	ParentReference struct {
		DriveID string `json:"driveId"`
		ID      string `json:"id"`
		Path    string `json:"path"`
		SiteID  string `json:"siteId"`
	} `json:"parentReference"`
}

func (d driveItem) toItem(driveID string) Item {
	it := Item{ID: d.ID, DriveID: driveID, Name: d.Name, WebURL: d.WebURL, Size: d.Size, Modified: d.Modified}
	if it.DriveID == "" {
		it.DriveID = d.ParentReference.DriveID
	}
	if d.Folder != nil {
		it.Folder = true
		it.ChildCount = d.Folder.ChildCount
	}
	return it
}

// ---- libraries ----

// Libraries lists every document library in every site the user can reach, plus their OneDrive.
// Results are cached in memory for 30 minutes.
func (g *Graph) Libraries(ctx context.Context, refresh bool) ([]Library, error) {
	g.libMu.Lock()
	defer g.libMu.Unlock()
	if !refresh && g.libs != nil && time.Since(g.libsTime) < 30*time.Minute {
		return g.libs, nil
	}

	type site struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Name        string `json:"name"`
		WebURL      string `json:"webUrl"`
	}
	var sites []site
	next := "/sites?search=*&$select=id,displayName,name,webUrl&$top=200"
	for next != "" {
		var page struct {
			Value []site `json:"value"`
			Next  string `json:"@odata.nextLink"`
		}
		if err := g.get(ctx, next, &page); err != nil {
			return nil, err
		}
		sites = append(sites, page.Value...)
		next = page.Next
	}

	var (
		mu   sync.Mutex
		libs []Library
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 8)
	)
	for _, s := range sites {
		wg.Add(1)
		go func(s site) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var page struct {
				Value []struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					WebURL    string `json:"webUrl"`
					DriveType string `json:"driveType"`
				} `json:"value"`
			}
			if err := g.get(ctx, "/sites/"+s.ID+"/drives?$select=id,name,webUrl,driveType", &page); err != nil {
				return // sites the user can see but not read: skip
			}
			name := s.DisplayName
			if name == "" {
				name = s.Name
			}
			mu.Lock()
			for _, d := range page.Value {
				if d.DriveType != "documentLibrary" {
					continue
				}
				libs = append(libs, Library{DriveID: d.ID, Site: name, SiteURL: s.WebURL, Name: d.Name, WebURL: d.WebURL, Kind: d.DriveType})
			}
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	var me struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		WebURL    string `json:"webUrl"`
		DriveType string `json:"driveType"`
	}
	if err := g.get(ctx, "/me/drive?$select=id,name,webUrl,driveType", &me); err == nil && me.ID != "" {
		libs = append(libs, Library{DriveID: me.ID, Site: "OneDrive", Name: me.Name, WebURL: me.WebURL, Kind: me.DriveType})
	}

	sort.Slice(libs, func(i, j int) bool {
		if !strings.EqualFold(libs[i].Site, libs[j].Site) {
			return strings.ToLower(libs[i].Site) < strings.ToLower(libs[j].Site)
		}
		return strings.ToLower(libs[i].Name) < strings.ToLower(libs[j].Name)
	})
	g.libs, g.libsTime = libs, time.Now()
	return libs, nil
}

// ---- children ----

func itemPath(driveID, itemID string) string {
	if itemID == "" || itemID == "root" {
		return "/drives/" + url.PathEscape(driveID) + "/root"
	}
	return "/drives/" + url.PathEscape(driveID) + "/items/" + url.PathEscape(itemID)
}

// Children lists every child of a folder (all pages).
func (g *Graph) Children(ctx context.Context, driveID, itemID string) ([]Item, error) {
	next := itemPath(driveID, itemID) + "/children?$select=id,name,webUrl,size,lastModifiedDateTime,folder,file&$top=999"
	var out []Item
	for next != "" {
		var page struct {
			Value []driveItem `json:"value"`
			Next  string      `json:"@odata.nextLink"`
		}
		if err := g.get(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, d := range page.Value {
			out = append(out, d.toItem(driveID))
		}
		next = page.Next
	}
	return out, nil
}

// Ancestors returns the folder IDs from just below the drive root down to (and including) itemID.
func (g *Graph) Ancestors(ctx context.Context, driveID, itemID string) ([]Item, error) {
	var chain []Item
	id := itemID
	for i := 0; i < 64 && id != ""; i++ {
		var d driveItem
		if err := g.get(ctx, itemPath(driveID, id)+"?$select=id,name,webUrl,size,folder,file,root,parentReference", &d); err != nil {
			return nil, err
		}
		if d.Root != nil {
			break
		}
		chain = append([]Item{d.toItem(driveID)}, chain...)
		id = d.ParentReference.ID
	}
	return chain, nil
}

// ---- search ----

func kqlTerms(q string) string {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '"', '(', ')', ':', '*', '\\':
			return ' '
		}
		return r
	}, q)
	var parts []string
	for _, t := range strings.Fields(clean) {
		switch strings.ToUpper(t) {
		case "AND", "OR", "NOT", "NEAR":
			continue
		}
		parts = append(parts, t+"*")
	}
	return strings.Join(parts, " ")
}

// Search finds folders (or files too) by name across SharePoint and OneDrive using the Microsoft Search API.
func (g *Graph) Search(ctx context.Context, q string, includeFiles bool, size int) ([]Item, error) {
	terms := kqlTerms(q)
	if terms == "" {
		return nil, nil
	}
	query := terms
	if !includeFiles {
		query += " IsContainer:true"
	}
	body := map[string]any{"requests": []map[string]any{{
		"entityTypes": []string{"driveItem"},
		"query":       map[string]string{"queryString": query},
		"from":        0,
		"size":        size,
	}}}
	var resp struct {
		Value []struct {
			HitsContainers []struct {
				Hits []struct {
					Resource struct {
						driveItem
						File *struct{} `json:"file"`
					} `json:"resource"`
				} `json:"hits"`
			} `json:"hitsContainers"`
		} `json:"value"`
	}
	if err := g.do(ctx, "POST", "/search/query", body, &resp); err != nil {
		return nil, err
	}
	var out []Item
	for _, v := range resp.Value {
		for _, hc := range v.HitsContainers {
			for _, h := range hc.Hits {
				r := h.Resource
				it := r.toItem(r.ParentReference.DriveID)
				if r.File == nil && !includeFiles {
					it.Folder = true
				}
				if it.DriveID == "" || it.ID == "" {
					continue
				}
				it.Path, it.Site = humanPath(it.WebURL)
				out = append(out, it)
			}
		}
	}
	return out, nil
}

// humanPath turns https://t.sharepoint.com/sites/x/Shared Documents/a/b/c into ("x › a › b", "x").
func humanPath(webURL string) (string, string) {
	u, err := url.Parse(webURL)
	if err != nil {
		return "", ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 3 {
		return "", ""
	}
	site := segs[1]
	rest := segs[3:]
	if len(rest) > 0 {
		rest = rest[:len(rest)-1]
	}
	return strings.Join(append([]string{site}, rest...), " › "), site
}

// Me returns the signed-in user's display name.
func (g *Graph) Me(ctx context.Context) (string, error) {
	var me struct {
		DisplayName       string `json:"displayName"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := g.get(ctx, "/me?$select=displayName,userPrincipalName", &me); err != nil {
		return "", err
	}
	if me.DisplayName != "" {
		return me.DisplayName, nil
	}
	return me.UserPrincipalName, nil
}

func isNeedLogin(err error) bool { return errors.Is(err, ErrNeedLogin) }
