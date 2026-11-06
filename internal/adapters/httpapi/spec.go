package httpapi

import (
	"net/http"

	"enquoir/api"
)

func serveSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	w.Write(api.Spec)
}

// docsPage renders /openapi.yaml with Redoc. It is a convenience for
// people reading the API; the contract itself is /openapi.yaml.
const docsPage = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>Enquoir API reference</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
</head>
<body>
  <redoc spec-url="/openapi.yaml"></redoc>
  <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
</body>
</html>
`

func serveDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(docsPage))
}
