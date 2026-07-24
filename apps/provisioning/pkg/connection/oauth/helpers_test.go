package oauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"

	provisioning "github.com/grafana/grafana/apps/provisioning/pkg/apis/provisioning/v0alpha1"
	"github.com/grafana/grafana/apps/provisioning/pkg/connection"
	"github.com/grafana/grafana/apps/provisioning/pkg/connection/oauth"
	common "github.com/grafana/grafana/pkg/apimachinery/apis/common/v0alpha1"
)

func TestDecryptSecrets(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(m *connection.MockSecureValues)
		expected    oauth.ConnectionSecrets
		expectedErr string
	}{
		{
			name: "success",
			setup: func(m *connection.MockSecureValues) {
				m.EXPECT().ClientSecret(mock.Anything).Return("client-secret", nil)
				m.EXPECT().Token(mock.Anything).Return("token", nil)
			},
			expected: oauth.ConnectionSecrets{
				ClientSecret: "client-secret",
				Token:        "token",
			},
		},
		{
			name: "failure - client secret decrypt error",
			setup: func(m *connection.MockSecureValues) {
				m.EXPECT().ClientSecret(mock.Anything).Return("", errors.New("boom"))
			},
			expectedErr: "decrypt client secret: boom",
		},
		{
			name: "failure - token decrypt error",
			setup: func(m *connection.MockSecureValues) {
				m.EXPECT().ClientSecret(mock.Anything).Return("client-secret", nil)
				m.EXPECT().Token(mock.Anything).Return("", errors.New("boom"))
			},
			expectedErr: "decrypt token: boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secure := connection.NewMockSecureValues(t)
			tt.setup(secure)
			decrypter := func(*provisioning.Connection) connection.SecureValues { return secure }

			secrets, err := oauth.DecryptSecrets(context.Background(), decrypter, &provisioning.Connection{})
			if tt.expectedErr != "" {
				require.EqualError(t, err, tt.expectedErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, secrets)
		})
	}
}

func TestValidateCredentials(t *testing.T) {
	tests := []struct {
		name       string
		connection *provisioning.Connection
		cfgPresent bool
		clientID   string
		expected   field.ErrorList
	}{
		{
			name: "valid",
			connection: &provisioning.Connection{
				Secure: provisioning.ConnectionSecure{
					ClientSecret: common.InlineSecureValue{Create: "client-secret"},
				},
			},
			cfgPresent: true,
			clientID:   "client-id",
		},
		{
			name:       "missing provider config",
			connection: &provisioning.Connection{},
			expected: field.ErrorList{
				field.Required(field.NewPath("spec", "gitlab"), "gitlab info must be specified in gitlab connection"),
				field.Required(field.NewPath("secure", "clientSecret"), "clientSecret must be specified for gitlab connection"),
			},
		},
		{
			name: "missing clientID",
			connection: &provisioning.Connection{
				Secure: provisioning.ConnectionSecure{
					ClientSecret: common.InlineSecureValue{Create: "client-secret"},
				},
			},
			cfgPresent: true,
			expected: field.ErrorList{
				field.Required(field.NewPath("spec", "gitlab", "clientID"), "clientID must be specified for gitlab connection"),
			},
		},
		{
			name:       "missing clientSecret",
			connection: &provisioning.Connection{},
			cfgPresent: true,
			clientID:   "client-id",
			expected: field.ErrorList{
				field.Required(field.NewPath("secure", "clientSecret"), "clientSecret must be specified for gitlab connection"),
			},
		},
		{
			name: "privateKey forbidden",
			connection: &provisioning.Connection{
				Secure: provisioning.ConnectionSecure{
					ClientSecret: common.InlineSecureValue{Create: "client-secret"},
					PrivateKey:   common.InlineSecureValue{Create: "private-key"},
				},
			},
			cfgPresent: true,
			clientID:   "client-id",
			expected: field.ErrorList{
				field.Forbidden(field.NewPath("secure", "privateKey"), "privateKey is forbidden in gitlab connection"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := oauth.ValidateCredentials(tt.connection, provisioning.GitlabConnectionType, tt.cfgPresent, tt.clientID)
			assert.Equal(t, tt.expected, errs)
		})
	}
}
