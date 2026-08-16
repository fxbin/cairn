package credential

import "testing"

func TestKeyUsesExpandedSharedVocabulary(t *testing.T) {
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "DEPLOY_PRIVATE_KEY", "DB_PASSWD", "GITHUB_PAT", "OPENAI_APIKEY", "SESSION_TOKEN"} {
		if !Key(name) {
			t.Fatalf("%q was not recognized as a credential key", name)
		}
	}
	for _, name := range []string{"PATH", "AUTH_TYPE", "HOME"} {
		if Key(name) {
			t.Fatalf("%q was unexpectedly recognized as a credential key", name)
		}
	}
}
