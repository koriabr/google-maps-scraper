package mailfilter_test

import (
	"testing"

	"github.com/gosom/google-maps-scraper/internal/mailfilter"
)

func TestIsPlaceholder(t *testing.T) {
	placeholders := []string{
		"seuemail@email.com", "SeuEmail@Email.com", "email@email.com", "nome@dominio.com.br",
		"seunome@gmail.com", "seu.nome@empresa.com.br", "your-email@gmail.com", "user@domain.com",
		"exemplo@exemplo.com.br", "test@test.com", "info@example.com", "a1b2c3@sentry.wixpress.com",
		"abc@o123.ingest.sentry.io", "logo@2x.png", "banner@3x.jpg", "sprite@fonts.woff2",
		"", "semarroba", "@dominio.com", "usuario@",
	}

	for _, email := range placeholders {
		if !mailfilter.IsPlaceholder(email) {
			t.Errorf("%q must be treated as a placeholder", email)
		}
	}

	real := []string{
		"contato@fretesdamasio.com.br", "vendas@transjoi.com.br", "financeiro@empresa.com.br",
		"joao.silva@gmail.com", "atendimento@lojaexemplo.com.br", "sac@email-marketing-empresa.com.br",
		"suporte@siteoficial.com.br", "nomes@empresa.com",
	}

	for _, email := range real {
		if mailfilter.IsPlaceholder(email) {
			t.Errorf("%q is a real contact and must be kept", email)
		}
	}
}
