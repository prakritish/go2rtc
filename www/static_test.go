package www

import (
	"bytes"
	"os"
	"testing"
)

// The API spec is served by the app (swagger/) and by the website (website/api/), the copies must be the same
func TestOpenAPICopies(t *testing.T) {
	embedded, err := Static.ReadFile("swagger/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	website, err := os.ReadFile("../website/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(embedded, website) {
		t.Fatal("www/swagger/openapi.yaml and website/api/openapi.yaml are different, copy one to the other")
	}
}

func TestSwaggerFiles(t *testing.T) {
	for _, name := range []string{"swagger/index.html", "swagger/swagger-ui-bundle.js", "swagger/swagger-ui.css"} {
		if _, err := Static.Open(name); err != nil {
			t.Error(err)
		}
	}
}
