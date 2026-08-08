package multiegg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fixtureTransport map[string]string

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := f[req.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("missing fixture")), Header: make(http.Header), Request: req}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
}

func TestPaperResolverContractFixture(t *testing.T) {
	fixtures := fixtureTransport{
		"https://fill.papermc.io/v3/projects/paper":                        `{"versions":{"1.21":["1.21.4"],"1.20":["1.20.6"]}}`,
		"https://fill.papermc.io/v3/projects/paper/versions/1.21.4/builds": `[{"id":12,"channel":"STABLE","downloads":{"server:default":{"name":"paper.jar","url":"https://fill-data.papermc.io/paper.jar","checksums":{"sha256":"` + strings.Repeat("a", 64) + `"}}}}]`,
	}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	spec := ProviderSpec{ID: "paper", Name: "Paper", Family: "bukkit", Architectures: []string{"amd64"}, Runtime: "java", Resolver: "papermc", Installer: "jar", Options: map[string]string{"project": "paper"}}
	r, err := NewProvider(spec).Resolve(context.Background(), Request{Version: "latest", Build: "latest", RuntimeVersion: "auto"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if r.Artifact.Version != "1.21.4" || r.Artifact.Build != "12" || !strings.Contains(r.Artifact.URL, "paper.jar") {
		t.Fatalf("%+v", r.Artifact)
	}
	if r.RuntimeVersion != "21" {
		t.Fatalf("runtime=%s", r.RuntimeVersion)
	}
}

func TestMojangResolverContractFixture(t *testing.T) {
	detail := "https://piston-meta.mojang.com/v/1.21.4.json"
	fixtures := fixtureTransport{
		"https://piston-meta.mojang.com/mc/game/version_manifest_v2.json": fmt.Sprintf(`{"latest":{"release":"1.21.4"},"versions":[{"id":"1.21.4","url":%q}]}`, detail),
		detail: `{"downloads":{"server":{"url":"https://piston-data.mojang.com/server.jar","sha1":"abcd","size":4}}}`,
	}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	spec := ProviderSpec{ID: "vanilla", Name: "Vanilla", Family: "vanilla", Architectures: []string{"amd64"}, Runtime: "java", Resolver: "mojang", Installer: "jar"}
	r, err := NewProvider(spec).Resolve(context.Background(), Request{Version: "latest", RuntimeVersion: "auto"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if r.Artifact.Version != "1.21.4" || r.Artifact.SHA1 != "abcd" {
		t.Fatalf("%+v", r.Artifact)
	}
}

func TestBedrockResolverContractFixture(t *testing.T) {
	fixtures := fixtureTransport{"https://net-secondary.web.minecraft-services.net/api/v1.0/download/links": `{"result":{"links":[{"downloadType":"serverBedrockLinux","downloadUrl":"https://www.minecraft.net/bedrockdedicatedserver/bin-linux/bedrock-server-1.26.36.1.zip"}]}}`}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	artifact, err := resolveBedrock(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.26.36.1" || !strings.HasSuffix(artifact.URL, ".zip") {
		t.Fatalf("%+v", artifact)
	}
}
