package githuboauth_test

import (
	"testing"

	provisioning "github.com/grafana/grafana/apps/provisioning/pkg/apis/provisioning/v0alpha1"
	"github.com/grafana/grafana/apps/provisioning/pkg/connection"
	"github.com/grafana/grafana/apps/provisioning/pkg/connection/githuboauth"
	common "github.com/grafana/grafana/pkg/apimachinery/apis/common/v0alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func mockDecrypter(t *testing.T) connection.Decrypter {
	return func(c *provisioning.Connection) connection.SecureValues {
		m := connection.NewMockSecureValues(t)
		m.EXPECT().ClientSecret(mock.Anything).Return(c.Secure.ClientSecret.Create, nil).Maybe()
		m.EXPECT().Token(mock.Anything).Return("", nil).Maybe()
		return m
	}
}

func TestExtra_Type(t *testing.T) {
	t.Run("should return GithubOAuthConnectionType", func(t *testing.T) {
		e := githuboauth.Extra(mockDecrypter(t))
		result := e.Type()
		assert.Equal(t, provisioning.GithubOAuthConnectionType, result)
	})
}

func TestExtra_Build(t *testing.T) {
	t.Run("should successfully build connection", func(t *testing.T) {
		ctx := t.Context()
		conn := &provisioning.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "test-connection"},
			Spec: provisioning.ConnectionSpec{
				Type: provisioning.GithubOAuthConnectionType,
				GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{
					ClientID: "test-client-id",
				},
			},
			Secure: provisioning.ConnectionSecure{
				ClientSecret: common.InlineSecureValue{
					Create: common.NewSecretValue("test-client-secret"),
				},
			},
		}

		e := githuboauth.Extra(mockDecrypter(t))

		result, err := e.Build(ctx, conn)
		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("should handle different connection configurations", func(t *testing.T) {
		ctx := t.Context()
		conn := &provisioning.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "another-connection"},
			Spec: provisioning.ConnectionSpec{
				Type: provisioning.GithubOAuthConnectionType,
				GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{
					ClientID: "another-client-id",
				},
			},
			Secure: provisioning.ConnectionSecure{
				ClientSecret: common.InlineSecureValue{
					Name: "existing-client-secret",
				},
			},
		}

		e := githuboauth.Extra(mockDecrypter(t))

		result, err := e.Build(ctx, conn)
		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("should build connection with background context", func(t *testing.T) {
		ctx := t.Context()
		conn := &provisioning.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "test-connection"},
			Spec: provisioning.ConnectionSpec{
				Type: provisioning.GithubOAuthConnectionType,
				GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{
					ClientID: "test-client-id",
				},
			},
		}

		e := githuboauth.Extra(mockDecrypter(t))
		result, err := e.Build(ctx, conn)
		require.NoError(t, err)
		require.NotNil(t, result)
	})
}

func TestExtra_Validate(t *testing.T) {
	tests := []struct {
		name          string
		obj           runtime.Object
		errorContains []string
	}{
		{
			name: "non-connection object",
			obj:  &runtime.Unknown{},
		},
		{
			name: "non-githubOAuth connection type",
			obj: &provisioning.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "test-conn"},
				Spec: provisioning.ConnectionSpec{
					Type: provisioning.GithubConnectionType,
				},
			},
		},
		{
			name: "githubOAuth connection type without githubOAuth config",
			obj: &provisioning.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "test-conn"},
				Spec: provisioning.ConnectionSpec{
					Type:        provisioning.GithubOAuthConnectionType,
					GitHubOAuth: nil,
				},
			},
			errorContains: []string{"githubOAuth info must be specified", "clientSecret"},
		},
		{
			name: "missing client ID",
			obj: &provisioning.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "test-conn"},
				Spec: provisioning.ConnectionSpec{
					Type:        provisioning.GithubOAuthConnectionType,
					GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{},
				},
				Secure: provisioning.ConnectionSecure{
					ClientSecret: common.InlineSecureValue{
						Create: common.NewSecretValue("test-secret"),
					},
				},
			},
			errorContains: []string{"clientID"},
		},
		{
			name: "missing client secret",
			obj: &provisioning.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "test-conn"},
				Spec: provisioning.ConnectionSpec{
					Type:        provisioning.GithubOAuthConnectionType,
					GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{ClientID: "test-client-id"},
				},
			},
			errorContains: []string{"clientSecret"},
		},
		{
			name: "forbidden private key",
			obj: &provisioning.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "test-conn"},
				Spec: provisioning.ConnectionSpec{
					Type:        provisioning.GithubOAuthConnectionType,
					GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{ClientID: "test-client-id"},
				},
				Secure: provisioning.ConnectionSecure{
					ClientSecret: common.InlineSecureValue{
						Create: common.NewSecretValue("test-secret"),
					},
					PrivateKey: common.InlineSecureValue{
						Create: common.NewSecretValue("test-key"),
					},
				},
			},
			errorContains: []string{"privateKey is forbidden"},
		},
		{
			name: "valid githubOAuth connection",
			obj: &provisioning.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "test-conn"},
				Spec: provisioning.ConnectionSpec{
					Type:        provisioning.GithubOAuthConnectionType,
					GitHubOAuth: &provisioning.GitHubOAuthConnectionConfig{ClientID: "test-client-id"},
				},
				Secure: provisioning.ConnectionSecure{
					ClientSecret: common.InlineSecureValue{
						Create: common.NewSecretValue("test-secret"),
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := githuboauth.Extra(mockDecrypter(t)).Validate(t.Context(), tt.obj)
			if len(tt.errorContains) == 0 {
				assert.Empty(t, list)
				return
			}
			require.NotEmpty(t, list)
			errStr := list.ToAggregate().Error()
			for _, contains := range tt.errorContains {
				assert.Contains(t, errStr, contains)
			}
		})
	}
}
