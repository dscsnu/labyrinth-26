package server

import (
	_ "embed"
	"net/http"
)

// openAPISpec is the API description, kept in step with the routes by
// TestOpenAPIMatchesRoutes.
//
//go:embed openapi.yaml
var openAPISpec []byte

// swaggerUIVersion pins the Swagger UI release the docs page loads from the
// CDN. A dependency version, not a tunable.
const swaggerUIVersion = "5.17.14"

// docsPage is the Swagger UI page for GET /docs. It loads Swagger UI from
// jsDelivr and points it at /openapi.yaml.
const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Labyrinth 26 API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css">
</head>
<body>
<div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({ url: "/openapi.yaml", dom_id: "#ui", persistAuthorization: true });</script>
</body>
</html>
`

// openAPI handles GET /openapi.yaml: the OpenAPI 3.1 description of every
// route.
func openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(openAPISpec)
}

// docs handles GET /docs: Swagger UI over /openapi.yaml.
func docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(docsPage))
}
