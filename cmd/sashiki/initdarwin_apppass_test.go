package main

import (
	"strings"
	"testing"
)

// darwin テンプレートは --app-pass を config に通す(#324: dev 固定で無視されていた)。
func TestDarwinTemplatesHonorAppPass(t *testing.T) {
	for name, tmpl := range map[string]string{"mysql": configDarwinTmpl, "postgres": configDarwinPostgresTmpl} {
		data, err := renderTmpl(tmpl, map[string]string{
			"Root": "/r", "MysqldBin": "/m", "PgBinDir": "/p", "AppPass": yamlQuote(`s3c"ret`)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(data), `app_pass: "s3c\"ret"`) {
			t.Errorf("%s: app_pass not rendered from AppPass:\n%s", name, data)
		}
	}
}
