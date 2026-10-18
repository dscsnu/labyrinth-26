package tests

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specDoc is the part of openapi.yaml these tests read.
type specDoc struct {
	Paths map[string]map[string]struct {
		Responses map[string]struct {
			Ref         string `yaml:"$ref"`
			Description string `yaml:"description"`
		} `yaml:"responses"`
	} `yaml:"paths"`
}

// undocumentedRoutes are served but deliberately not in the spec: the spec
// itself and its viewer.
var undocumentedRoutes = []string{"GET /openapi.yaml", "GET /docs"}

// unroutedCodes are sent by the server for requests no route matches; the
// spec describes them once, in info.description, not per path.
var unroutedCodes = []string{"not_found", "method_not_allowed"}

// statusByName is the net/http constants the API uses for error codes. A new
// one fails the test until it's added here and to the spec.
var statusByName = map[string]int{
	"StatusBadRequest": 400, "StatusUnauthorized": 401, "StatusForbidden": 403,
	"StatusNotFound": 404, "StatusMethodNotAllowed": 405, "StatusConflict": 409,
}

// loadSpec reads and parses openapi.yaml, returning its raw text too.
func loadSpec(t *testing.T) (specDoc, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	return doc, string(raw)
}

// sourceFiles returns the contents of every non-test Go file under internal/.
func sourceFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		files[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestOpenAPIMatchesRoutes checks that openapi.yaml documents exactly the
// routes the code registers, every method and path.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	doc, _ := loadSpec(t)
	route := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) (/[^"]*)"`)

	var registered []string
	for _, src := range sourceFiles(t) {
		for _, m := range route.FindAllStringSubmatch(src, -1) {
			if r := m[1] + " " + m[2]; !slices.Contains(undocumentedRoutes, r) {
				registered = append(registered, r)
			}
		}
	}
	var documented []string
	for path, ops := range doc.Paths {
		for method := range ops {
			documented = append(documented, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(registered)
	slices.Sort(documented)
	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("route %s is registered but not in openapi.yaml", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(registered, d) {
			t.Errorf("openapi.yaml documents %s, which no code registers", d)
		}
	}
}

// TestOpenAPIErrorCodes checks that every error code the code can send is in
// openapi.yaml under its status, and that the spec lists no code the code
// never sends.
func TestOpenAPIErrorCodes(t *testing.T) {
	doc, raw := loadSpec(t)
	// httpx.ErrorCode(w, http.StatusX, "code", …) and errorCodes rows
	// {ErrX, http.StatusX, "code"}.
	sent := regexp.MustCompile(`http\.(Status[A-Za-z]+),\s*"([a-z_]+)"`)

	// A code may be sent with different statuses by different routes
	// (user_not_found: 401 from /api/me, 404 from /auth/token).
	inCode := map[string][]int{} // code → statuses it's sent with
	for path, src := range sourceFiles(t) {
		for _, m := range sent.FindAllStringSubmatch(src, -1) {
			status, ok := statusByName[m[1]]
			if !ok {
				t.Fatalf("%s: %s %q: add the status to statusByName and the spec", path, m[1], m[2])
			}
			if !slices.Contains(inCode[m[2]], status) {
				inCode[m[2]] = append(inCode[m[2]], status)
			}
		}
	}

	listed := regexp.MustCompile("Codes: (.*)")
	tick := regexp.MustCompile("`([a-z_]+)`")
	inSpec := map[string][]int{} // code → statuses it's listed under
	for _, ops := range doc.Paths {
		for _, op := range ops {
			for status, resp := range op.Responses {
				m := listed.FindStringSubmatch(resp.Description)
				if m == nil {
					continue
				}
				n, _ := strconv.Atoi(status)
				for _, c := range tick.FindAllStringSubmatch(m[1], -1) {
					inSpec[c[1]] = append(inSpec[c[1]], n)
				}
			}
		}
	}

	for code, statuses := range inCode {
		if slices.Contains(unroutedCodes, code) {
			if !strings.Contains(raw, "`"+code+"`") {
				t.Errorf("unrouted code %q isn't described in openapi.yaml", code)
			}
			continue
		}
		for _, status := range statuses {
			if !slices.Contains(inSpec[code], status) {
				t.Errorf("code %q is sent with %d but isn't listed under %d in openapi.yaml (listed under %v)", code, status, status, inSpec[code])
			}
		}
	}
	for code, statuses := range inSpec {
		sentWith, ok := inCode[code]
		if !ok {
			t.Errorf("openapi.yaml lists code %q, which the code never sends", code)
			continue
		}
		for _, s := range statuses {
			if !slices.Contains(sentWith, s) {
				t.Errorf("openapi.yaml lists %q under %d; the code sends it with %v", code, s, sentWith)
			}
		}
	}
}

// TestOpenAPIRefsResolve checks that every $ref in openapi.yaml points at a
// component that exists.
func TestOpenAPIRefsResolve(t *testing.T) {
	_, raw := loadSpec(t)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	components := doc["components"].(map[string]any)
	for _, m := range regexp.MustCompile(`\$ref: "#/components/([a-zA-Z]+)/([A-Za-z]+)"`).FindAllStringSubmatch(raw, -1) {
		section, _ := components[m[1]].(map[string]any)
		if _, ok := section[m[2]]; !ok {
			t.Errorf("$ref #/components/%s/%s doesn't exist", m[1], m[2])
		}
	}
}

// TestDocsServed checks that the spec and the Swagger UI page are served
// without a token.
func TestDocsServed(t *testing.T) {
	e := setup(t)
	res := e.get("/openapi.yaml")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/yaml" {
		t.Fatalf("/openapi.yaml: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res := e.get("/docs"); res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/docs: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
}
