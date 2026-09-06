/*
╔═ main.go ═══════════════════════════════════════════════════════════════════════════
║  tooling · openapi generator
╠═ reached from ══════════════════════════════════════════════════════════════════════
║      go run ./cmd/openapi  >  api/openapi.json
║      ci.yml                →  drift check
╚═════════════════════════════════════════════════════════════════════════════════════
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/api"
)

/*
┌─ openapi ───────────────────────────────────────
│  prints the OpenAPI document, indented, to stdout
├─ out ───────────────────────────────────────────
│      exit 0     spec on stdout
│      exit 1     message on stderr
├─ example ───────────────────────────────────────
│      go run ./cmd/openapi  →  153 lines of JSON
*/
func main() {
	spec, err := api.NewHumaAPI(http.NewServeMux()).OpenAPI().MarshalJSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
		os.Exit(1)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, spec, "", "  "); err != nil {
		fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
		os.Exit(1)
	}
	pretty.WriteString("\n")
	if _, err := pretty.WriteTo(os.Stdout); err != nil {
		os.Exit(1)
	}
}
