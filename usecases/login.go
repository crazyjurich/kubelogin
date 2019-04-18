package usecases

import (
	"context"

	"github.com/coreos/go-oidc"
	"github.com/int128/kubelogin/adaptors/interfaces"
	"github.com/int128/kubelogin/usecases/interfaces"
	"github.com/pkg/errors"
	"go.uber.org/dig"
)

const oidcConfigErrorMessage = `No OIDC configuration found. Did you setup kubectl for OIDC authentication?
  kubectl config set-credentials %[1]s \
    --auth-provider oidc \
    --auth-provider-arg idp-issuer-url=https://issuer.example.com \
    --auth-provider-arg client-id=YOUR_CLIENT_ID \
    --auth-provider-arg client-secret=YOUR_CLIENT_SECRET`

func NewLogin(i Login) usecases.Login {
	return &i
}

type Login struct {
	dig.In
	KubeConfig adaptors.KubeConfig
	HTTP       adaptors.HTTP
	OIDC       adaptors.OIDC
	Logger     adaptors.Logger
}

func (u *Login) Do(ctx context.Context, in usecases.LoginIn) error {
	u.Logger.Debugf(1, "WARNING: Log may contain your secrets, e.g. token or password")

	u.Logger.Debugf(1, "Loading %s", in.KubeConfigFilename)
	kubeConfig, err := u.KubeConfig.LoadFromFile(in.KubeConfigFilename)
	if err != nil {
		return errors.Wrapf(err, "could not read the kubeconfig")
	}

	kubeContextName := in.KubeContextName
	if kubeContextName == "" {
		kubeContextName = kubeConfig.CurrentContext
	}
	kubeContext, err := kubeConfig.FindContext(kubeContextName)
	if err != nil {
		return errors.Wrapf(err, "could not find the context %s", kubeContextName)
	}
	authProvider, err := kubeConfig.FindOIDCAuthProvider(kubeContext.AuthInfo)
	if err != nil {
		u.Logger.Printf(oidcConfigErrorMessage, kubeContextName)
		return errors.Wrapf(err, "could not find the oidc auth-provider")
	}
	u.Logger.Printf("Using user %s of context %s", kubeContext.AuthInfo, kubeContextName)

	clientConfig := u.HTTP.NewClientConfig()
	clientConfig.SetSkipTLSVerify(in.SkipTLSVerify)
	if authProvider.IDPCertificateAuthority() != "" {
		filename := authProvider.IDPCertificateAuthority()
		u.Logger.Printf("Using the certificate %s", filename)
		if err := clientConfig.AddCertificateFromFile(filename); err != nil {
			u.Logger.Printf("Skip the certificate %s: %s", filename, err)
		}
	}
	if authProvider.IDPCertificateAuthorityData() != "" {
		encoded := authProvider.IDPCertificateAuthorityData()
		u.Logger.Printf("Using the certificate of idp-certificate-authority-data")
		if err := clientConfig.AddEncodedCertificate(encoded); err != nil {
			u.Logger.Printf("Skip the certificate of idp-certificate-authority-data: %s", err)
		}
	}
	hc, err := u.HTTP.NewClient(clientConfig)
	if err != nil {
		return errors.Wrapf(err, "could not create a HTTP client")
	}

	if token := u.verifyIDToken(ctx, adaptors.OIDCVerifyTokenIn{
		IDToken:  authProvider.IDToken(),
		Issuer:   authProvider.IDPIssuerURL(),
		ClientID: authProvider.ClientID(),
		Client:   hc,
	}); token != nil {
		u.Logger.Printf("You already have a valid token (until %s)", token.Expiry)
		return nil
	}

	out, err := u.OIDC.Authenticate(ctx,
		adaptors.OIDCAuthenticateIn{
			Issuer:          authProvider.IDPIssuerURL(),
			ClientID:        authProvider.ClientID(),
			ClientSecret:    authProvider.ClientSecret(),
			ExtraScopes:     authProvider.ExtraScopes(),
			Client:          hc,
			LocalServerPort: in.ListenPort,
			SkipOpenBrowser: in.SkipOpenBrowser,
		},
		adaptors.OIDCAuthenticateCallback{
			ShowLocalServerURL: func(url string) {
				u.Logger.Printf("Open %s for authentication", url)
			},
		})
	if err != nil {
		return errors.Wrapf(err, "could not get token from OIDC provider")
	}

	u.Logger.Printf("Got a token for subject %s (valid until %s)", out.VerifiedIDToken.Subject, out.VerifiedIDToken.Expiry)
	u.Logger.Debugf(1, "Got an ID token %+v", out.VerifiedIDToken)
	authProvider.SetIDToken(out.IDToken)
	authProvider.SetRefreshToken(out.RefreshToken)

	u.Logger.Debugf(1, "Writing the ID token and refresh token to %s", in.KubeConfigFilename)
	if err := u.KubeConfig.WriteToFile(kubeConfig, in.KubeConfigFilename); err != nil {
		return errors.Wrapf(err, "could not update the kubeconfig")
	}
	u.Logger.Printf("Updated %s", in.KubeConfigFilename)
	return nil
}

func (u *Login) verifyIDToken(ctx context.Context, in adaptors.OIDCVerifyTokenIn) *oidc.IDToken {
	if in.IDToken == "" {
		return nil
	}
	token, err := u.OIDC.VerifyIDToken(ctx, in)
	if err != nil {
		u.Logger.Debugf(1, "Could not verify the ID token in the kubeconfig: %s", err)
		return nil
	}
	u.Logger.Debugf(1, "Verified token %+v", token)
	return token
}
