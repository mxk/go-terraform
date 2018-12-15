package tfaws

import (
	"sync"

	"github.com/LuminalHQ/cloudcover/x/tfx"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
	tfaws "github.com/terraform-providers/terraform-provider-aws/aws"
)

// ProviderName is the canonical name for the AWS provider.
const ProviderName = "aws"

func init() {
	version := tfx.ProviderVersion(tfaws.Provider)
	if version == "" {
		panic("tfaws: unknown provider version")
	}
	tfx.Providers.Add(ProviderName, version, factory)
}

// SessionLoader is called from ConfigureFunc to load default provider config.
var SessionLoader = func() (*session.Session, error) {
	return session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	})
}

var (
	once    sync.Once
	sess    *session.Session
	sessErr error
)

func factory() (tf.ResourceProvider, error) {
	p := tfx.InitSchemaProvider(tfaws.Provider())
	p.ConfigureFunc = func(d *schema.ResourceData) (interface{}, error) {
		if SessionLoader == nil {
			return nil, nil
		}
		once.Do(func() { sess, sessErr = SessionLoader() })
		if sessErr != nil {
			return nil, sessErr
		}
		cr, err := sess.Config.Credentials.Get()
		if err == nil && cr.SessionToken != "" {
			sess.Config.Credentials.Expire() // Force refresh
			cr, err = sess.Config.Credentials.Get()
		}
		if err != nil {
			return nil, err
		}
		cfg := tfaws.Config{
			AccessKey:            cr.AccessKeyID,
			SecretKey:            cr.SecretAccessKey,
			Token:                cr.SessionToken,
			Region:               d.Get("region").(string),
			MaxRetries:           d.Get("max_retries").(int),
			SkipGetEC2Platforms:  true,
			SkipMetadataApiCheck: true,

			// CredsValidation is needed to get partition and account id
			// RegionValidation doesn't make API calls
			// RequestingAccountId is ignored if CredsValidation works
		}
		if cfg.Region == "" {
			cfg.Region = *sess.Config.Region
		}
		return cfg.Client()
	}
	return p, nil
}
