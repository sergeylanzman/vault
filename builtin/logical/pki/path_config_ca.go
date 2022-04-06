package pki

import (
	"context"
	"fmt"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/certutil"
	"github.com/hashicorp/vault/sdk/helper/errutil"
	"github.com/hashicorp/vault/sdk/logical"
)

func pathConfigCA(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "config/ca",
		Fields: map[string]*framework.FieldSchema{
			"pem_bundle": {
				Type: framework.TypeString,
				Description: `PEM-format, concatenated unencrypted
secret key and certificate.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathCAWrite,
		},

		HelpSynopsis:    pathConfigCAHelpSyn,
		HelpDescription: pathConfigCAHelpDesc,
	}
}

func (b *backend) pathCAWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	pemBundle := data.Get("pem_bundle").(string)

	if pemBundle == "" {
		return logical.ErrorResponse("'pem_bundle' was empty"), nil
	}

	parsedBundle, err := certutil.ParsePEMBundle(pemBundle)
	if err != nil {
		switch err.(type) {
		case errutil.InternalError:
			return nil, err
		default:
			return logical.ErrorResponse(err.Error()), nil
		}
	}

	if parsedBundle.PrivateKey == nil {
		return logical.ErrorResponse("private key not found in the PEM bundle"), nil
	}

	if parsedBundle.PrivateKeyType == certutil.UnknownPrivateKey {
		return logical.ErrorResponse("unknown private key found in the PEM bundle"), nil
	}

	if parsedBundle.Certificate == nil {
		return logical.ErrorResponse("no certificate found in the PEM bundle"), nil
	}

	if !parsedBundle.Certificate.IsCA {
		return logical.ErrorResponse("the given certificate is not marked for CA use and cannot be used with this backend"), nil
	}

	cb, err := parsedBundle.ToCertBundle()
	if err != nil {
		return nil, fmt.Errorf("error converting raw values into cert bundle: %w", err)
	}

	entry, err := logical.StorageEntryJSON("config/ca_bundle", cb)
	if err != nil {
		return nil, err
	}
	err = req.Storage.Put(ctx, entry)
	if err != nil {
		return nil, err
	}

	// For ease of later use, also store just the certificate at a known
	// location, plus a fresh CRL
	entry.Key = "ca"
	entry.Value = parsedBundle.CertificateBytes
	err = req.Storage.Put(ctx, entry)
	if err != nil {
		return nil, err
	}

	err = buildCRL(ctx, b, req, true)

	return nil, err
}

const pathConfigCAHelpSyn = `
Set the CA certificate and private key used for generated credentials.
`

const pathConfigCAHelpDesc = `
This sets the CA information used for credentials generated by this
by this mount. This must be a PEM-format, concatenated unencrypted
secret key and certificate.

For security reasons, the secret key cannot be retrieved later.
`

func pathConfigIssuers(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "config/issuers",
		Fields: map[string]*framework.FieldSchema{
			"default": {
				Type:        framework.TypeString,
				Description: `Reference (name or identifier) to the default issuer.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.ReadOperation:   b.pathCAIssuersRead,
			logical.UpdateOperation: b.pathCAIssuersWrite,
		},

		HelpSynopsis:    pathConfigIssuersHelpSyn,
		HelpDescription: pathConfigIssuersHelpDesc,
	}
}

func (b *backend) pathCAIssuersRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	config, err := getIssuersConfig(ctx, req.Storage)
	if err != nil {
		return logical.ErrorResponse("Error loading issuers configuration: " + err.Error()), nil
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"default": config.DefaultIssuerId,
		},
	}, nil
}

func (b *backend) pathCAIssuersWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	newDefault := data.Get("default").(string)
	if len(newDefault) == 0 || newDefault == "default" {
		return logical.ErrorResponse("Invalid issuer specification; must be non-empty and can't be 'default'."), nil
	}

	parsedIssuer, err := resolveIssuerReference(ctx, req.Storage, newDefault)
	if err != nil {
		return logical.ErrorResponse("Error resolving issuer reference: " + err.Error()), nil
	}

	err = updateDefaultIssuerId(ctx, req.Storage, parsedIssuer)
	if err != nil {
		return logical.ErrorResponse("Error updating issuer configuration: " + err.Error()), nil
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"default": parsedIssuer,
		},
	}, nil
}

const pathConfigIssuersHelpSyn = `Read and set the default issuer certificate for signing.`

const pathConfigIssuersHelpDesc = `
This path allows configuration of issuer parameters.

Presently, the "default" parameter controls which issuer is the default,
accessible by the existing signing paths (/root/sign-intermediate,
/root/sign-self-issued, /sign-verbatim, /sign/:role, and /issue/:role).
`

const pathConfigCAGenerateHelpSyn = `
Generate a new CA certificate and private key used for signing.
`

const pathConfigCAGenerateHelpDesc = `
This path generates a CA certificate and private key to be used for
credentials generated by this mount. The path can either
end in "internal" or "exported"; this controls whether the
unencrypted private key is exported after generation. This will
be your only chance to export the private key; for security reasons
it cannot be read or exported later.

If the "type" option is set to "self-signed", the generated
certificate will be a self-signed root CA. Otherwise, this mount
will act as an intermediate CA; a CSR will be returned, to be signed
by your chosen CA (which could be another mount of this backend).
Note that the CRL path will be set to this mount's CRL path; if you
need further customization it is recommended that you create a CSR
separately and get it signed. Either way, use the "config/ca/set"
endpoint to load the signed certificate into Vault.
`

const pathConfigCASignHelpSyn = `
Generate a signed CA certificate from a CSR.
`

const pathConfigCASignHelpDesc = `
This path generates a CA certificate to be used for credentials
generated by the certificate's destination mount.

Use the "config/ca/set" endpoint to load the signed certificate
into Vault another Vault mount.
`
