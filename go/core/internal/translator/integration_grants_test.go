package translator_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/kube/krt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/kagent-dev/kagent/go/core/pkg/egresspolicy"
)

func TestAppliedIntegrationResolution(t *testing.T) {
	for _, kind := range []string{egress.IntegrationModelConfig, egress.IntegrationRemoteMCPServer} {
		for _, test := range []struct {
			name    string
			allowed bool
			issue   bool
		}{
			{name: "unchanged", allowed: true},
			{name: "deleted"},
			{name: "replaced"},
			{name: "removed headers"},
			{name: "changed binding"},
			{name: "invalid headers", allowed: true, issue: true},
			{name: "revoked then invalid", issue: true},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				target := egresspolicy.GatewayTarget{Origin: "https://api.openai.com:443", Header: "authorization", Prefix: "Bearer "}
				grant := egress.IntegrationGrant{Kind: kind, Name: "integration", UID: "applied-uid", Targets: []egresspolicy.GatewayTarget{target}}
				metadata := metav1.ObjectMeta{Namespace: "test", Name: grant.Name, UID: "applied-uid"}
				headers := []v1alpha3.Header{{Name: "Authorization", Prefix: new("Bearer "), ValueFrom: v1alpha3.HeaderValueSource{TokenPassthrough: &v1alpha3.TokenPassthrough{}}}}
				switch test.name {
				case "replaced":
					metadata.UID = "replacement"
				case "removed headers":
					headers = nil
				case "changed binding":
					headers[0].Prefix = new("Token ")
				case "invalid headers", "revoked then invalid":
					headers[0].Prefix = new("invalid\r\n")
				}
				if test.name == "revoked then invalid" {
					grant.RevokedTargets = grant.Targets
				}
				var objects []any
				if test.name != "deleted" {
					if kind == egress.IntegrationModelConfig {
						objects = append(objects, &v1alpha3.ModelConfig{ObjectMeta: metadata, Spec: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Headers: headers}})
					} else {
						objects = append(objects, &v1alpha3.RemoteMCPServer{ObjectMeta: metadata, Spec: v1alpha3.RemoteMCPServerSpec{URL: "https://api.openai.com/mcp", Headers: headers}})
					}
				}
				resolved, _ := translator.ResolveIntegrationGrants(krt.TestingDummyContext{}, mockCollections(t, objects...), "test", []egress.IntegrationGrant{grant})
				if test.allowed {
					require.Equal(t, []egresspolicy.GatewayTarget{target}, resolved.Access.TokenPassthroughTargets)
				} else {
					require.Empty(t, resolved.Access.TokenPassthroughTargets)
				}
				require.Equal(t, test.issue, len(resolved.Issues) > 0)
			})
		}
	}
}
