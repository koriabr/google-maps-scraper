package gmaps

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestEmailExtractorsSkipPlaceholders(t *testing.T) {
	html := `<html><body>
	  <a href="mailto:contato@fretesdamasio.com.br">Fale conosco</a>
	  <a href="mailto:seuemail@email.com">exemplo</a>
	  <p>Escreva para vendas@fretesdamasio.com.br ou seuemail@email.com ou logo@2x.png</p>
	</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}

	for name, got := range map[string][]string{
		"mailto links": docEmailExtractor(doc),
		"page text":    regexEmailExtractor([]byte(html)),
	} {
		for _, email := range got {
			if strings.Contains(email, "seuemail") || strings.HasSuffix(email, ".png") {
				t.Fatalf("%s kept a placeholder: %q", name, got)
			}
		}

		if len(got) == 0 {
			t.Fatalf("%s found no real e-mail at all", name)
		}
	}
}
