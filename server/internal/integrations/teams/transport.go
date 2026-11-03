package teams

import (
	"net/http"
	"time"
)

// Transport is the process-wide outbound plumbing every Teams component
// shares: the secret decrypter, one token cache and one HTTP client. Build it
// once in the wiring and hand it to the webhook, replier, outbound and typing
// manager so they mint tokens through the same cache.
type Transport struct {
	decrypt Decrypter
	tokens  *tokenSource
	client  *http.Client
}

// NewTransport builds the production transport.
func NewTransport(decrypt Decrypter) *Transport {
	return newTransport(decrypt, "", nil)
}

// newTransport lets tests point token minting at an httptest server.
func newTransport(decrypt Decrypter, loginBase string, client *http.Client) *Transport {
	if client == nil {
		client = &http.Client{Timeout: connectorTimeout}
	}
	return &Transport{
		decrypt: decrypt,
		tokens:  newTokenSource(loginBase, client),
		client:  client,
	}
}

// connectorFor decodes an installation config blob into a ready connector.
func (t *Transport) connectorFor(config []byte) (*connector, credentials, error) {
	creds, err := decodeCredentials(config, t.decrypt)
	if err != nil {
		return nil, credentials{}, err
	}
	return newConnector(creds, t.tokens, t.client), creds, nil
}

// detachedTimeout bounds best-effort sends that run off the request path.
const detachedTimeout = 30 * time.Second
