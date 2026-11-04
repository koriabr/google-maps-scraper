// Package mailfilter recognises e-mail addresses that are not real contacts:
// the placeholders sites leave in forms and footers ("seuemail@email.com"),
// reserved example domains, error-tracking addresses injected by site builders
// and file names that merely look like addresses ("logo@2x.png").
package mailfilter

import "strings"

// placeholderLocals are local parts that only ever appear as an example.
var placeholderLocals = map[string]struct{}{
	"seuemail": {}, "seu-email": {}, "seu_email": {}, "seu.email": {},
	"youremail": {}, "your-email": {}, "your_email": {}, "your.email": {},
	"seunome": {}, "seu-nome": {}, "seu_nome": {}, "seu.nome": {},
	"yourname": {}, "your-name": {}, "your_name": {}, "your.name": {},
	"nome": {}, "name": {}, "nomedoemail": {}, "email": {}, "e-mail": {},
	"usuario": {}, "username": {}, "user": {},
	"exemplo": {}, "example": {}, "teste": {}, "test": {},
}

// placeholderDomains are example, reserved or tracking domains. A subdomain of
// any of them is also treated as a placeholder.
var placeholderDomains = []string{
	"email.com", "email.com.br",
	"example.com", "example.org", "example.net", "exemplo.com", "exemplo.com.br",
	"domain.com", "yourdomain.com", "dominio.com", "dominio.com.br",
	"seudominio.com", "seudominio.com.br", "seusite.com", "seusite.com.br",
	"mysite.com", "yoursite.com", "site.com", "test.com", "teste.com", "teste.com.br",
	// Error tracking injected into pages by site builders.
	"sentry.io", "wixpress.com",
}

// fileExtensions are TLDs that are really file names ("logo@2x.png").
var fileExtensions = map[string]struct{}{
	"png": {}, "jpg": {}, "jpeg": {}, "gif": {}, "svg": {}, "webp": {},
	"ico": {}, "css": {}, "js": {}, "woff": {}, "woff2": {}, "ttf": {}, "map": {},
}

// IsPlaceholder reports whether the address is not a real contact.
func IsPlaceholder(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))

	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" {
		return true
	}

	if _, ok := placeholderLocals[local]; ok {
		return true
	}

	if dot := strings.LastIndex(domain, "."); dot >= 0 {
		if _, ok := fileExtensions[domain[dot+1:]]; ok {
			return true
		}
	}

	for _, blocked := range placeholderDomains {
		if domain == blocked || strings.HasSuffix(domain, "."+blocked) {
			return true
		}
	}

	return false
}
