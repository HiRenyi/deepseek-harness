package cdp

// DOMDocument represents the response from DOM.getDocument.
type DOMDocument struct {
	Root *DOMNode `json:"root,omitempty"`
}

// DOMNode represents a single DOM node from CDP.
type DOMNode struct {
	NodeID          int              `json:"nodeId"`
	BackendNodeID   int              `json:"backendNodeId"`
	NodeType        int              `json:"nodeType"`
	NodeName        string           `json:"nodeName"`
	LocalName       string           `json:"localName"`
	NodeValue       string           `json:"nodeValue"`
	Attributes      []string         `json:"attributes,omitempty"`
	ChildNodeCount  int              `json:"childNodeCount,omitempty"`
	Children        []*DOMNode       `json:"children,omitempty"`
	DocumentURL     string           `json:"documentURL,omitempty"`
	BaseURL         string           `json:"baseURL,omitempty"`
	PublicID        string           `json:"publicId,omitempty"`
	SystemID        string           `json:"systemId,omitempty"`
	InternalSubset  string           `json:"internalSubset,omitempty"`
	XMLVersion      string           `json:"xmlVersion,omitempty"`
	Name            string           `json:"name,omitempty"`
	Value           string           `json:"value,omitempty"`
	PseudoType      string           `json:"pseudoType,omitempty"`
	ShadowRootType  string           `json:"shadowRootType,omitempty"`
	FrameID         string           `json:"frameId,omitempty"`
	ContentDocument *DOMNode         `json:"contentDocument,omitempty"`
	ShadowRoots     []*DOMNode       `json:"shadowRoots,omitempty"`
	TemplateContent *DOMNode         `json:"templateContent,omitempty"`
	PseudoElements  []*DOMNode       `json:"pseudoElements,omitempty"`
	IsSVG           bool             `json:"isSVG,omitempty"`
}

// TagName returns the lowercase tag name of the node.
func (n *DOMNode) TagName() string {
	return stringsToLower(n.NodeName)
}

// GetAttributes converts the flat attribute array [name1,val1,name2,val2,...] to a map.
func (n *DOMNode) GetAttributes() map[string]string {
	attrs := make(map[string]string, len(n.Attributes)/2)
	for i := 0; i+1 < len(n.Attributes); i += 2 {
		attrs[n.Attributes[i]] = n.Attributes[i+1]
	}
	return attrs
}

// LayoutMetrics represents the response from Page.getLayoutMetrics.
type LayoutMetrics struct {
	CSSLayoutViewport  *LayoutViewport  `json:"cssLayoutViewport,omitempty"`
	CSSVisualViewport  *VisualViewport  `json:"cssVisualViewport,omitempty"`
	CSSContentSize     *ViewportRect    `json:"cssContentSize,omitempty"`
	LayoutViewport     *LayoutViewport  `json:"layoutViewport,omitempty"`
	VisualViewport     *VisualViewport  `json:"visualViewport,omitempty"`
	ContentSize        *ViewportRect    `json:"contentSize,omitempty"`
}

// LayoutViewport represents CSS layout viewport metrics.
type LayoutViewport struct {
	PageX        int `json:"pageX"`
	PageY        int `json:"pageY"`
	ClientWidth  int `json:"clientWidth"`
	ClientHeight int `json:"clientHeight"`
}

// VisualViewport represents CSS visual viewport metrics.
type VisualViewport struct {
	OffsetX      float64 `json:"offsetX"`
	OffsetY      float64 `json:"offsetY"`
	PageX        float64 `json:"pageX"`
	PageY        float64 `json:"pageY"`
	ClientWidth  float64 `json:"clientWidth"`
	ClientHeight float64 `json:"clientHeight"`
	Scale        float64 `json:"scale"`
	Zoom         float64 `json:"zoom"`
}

// ViewportRect represents a rectangular area.
type ViewportRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// stringsToLower is a simple ASCII-only toLower to avoid importing strings.
func stringsToLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// Quad is a CDP quad array: [x1,y1, x2,y2, x3,y3, x4,y4] (clockwise corners).
// Aligned with Codex UT(quad): center = average of four x and four y.
type Quad []float64

// Center returns the geometric center of the quad (average of four corners).
// ok is false if the quad has fewer than 8 elements.
func (q Quad) Center() (x, y float64, ok bool) {
	if len(q) < 8 {
		return 0, 0, false
	}
	return (q[0] + q[2] + q[4] + q[6]) / 4, (q[1] + q[3] + q[5] + q[7]) / 4, true
}

// BoxModelResponse is the response from DOM.getBoxModel.
type BoxModelResponse struct {
	Model struct {
		Content Quad `json:"content"`
		Padding Quad `json:"padding"`
		Border  Quad `json:"border"`
		Margin  Quad `json:"margin"`
	} `json:"model"`
}

// ContentQuadsResponse is the response from DOM.getContentQuads.
type ContentQuadsResponse struct {
	Quads []Quad `json:"quads"`
}

// NavigateResult is the response from Page.navigate, enriched with Loaded status.
type NavigateResult struct {
	FrameID   string `json:"frameId"`
	LoaderID  string `json:"loaderId"`
	ErrorText string `json:"errorText,omitempty"`
	// Loaded indicates whether document.readyState reached "complete" within the timeout.
	// This field is set by Navigate (not part of the raw CDP response).
	Loaded bool `json:"-"`
	// HTTPStatus is the main-frame HTTP status code probed via
	// PerformanceNavigationTiming.responseStatus after load (bridge enhancement
	// beyond Codex, which only checks Page.navigate errorText and misses 4xx/5xx).
	// -1 = unknown (eval failed, no navigation entry, or cross-origin restricted).
	HTTPStatus int `json:"-"`
}

// ScreenshotResult is the response from Page.captureScreenshot.
type ScreenshotResult struct {
	Data string `json:"data"` // base64-encoded image data
}

// ResolveNodeResult is the response from DOM.resolveNode.
type ResolveNodeResult struct {
	Object struct {
		ObjectID string `json:"objectId"`
	} `json:"object"`
}

// CallFunctionOnResult is the response from Runtime.callFunctionOn.
type CallFunctionOnResult struct {
	Result struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"result"`
}

// GetAttributesResult is the response from DOM.getAttributes (flat array).
type GetAttributesResult struct {
	Attributes []string `json:"attributes"`
}

// NavigationHistoryResult is the response from Page.getNavigationHistory.
type NavigationHistoryResult struct {
	Entries     []NavigationHistoryEntry `json:"entries"`
	CurrentIndex int                     `json:"currentIndex"`
}

// NavigationHistoryEntry is a single entry in the browser navigation history.
type NavigationHistoryEntry struct {
	ID    int    `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}
