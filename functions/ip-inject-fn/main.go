// Package main implements a Composition Function.
package main

import (
	"os"

	"github.com/alecthomas/kong"

	"github.com/crossplane/function-sdk-go"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type CLI struct {
	Debug bool `short:"d" help:"Emit debug logs in addition to info logs."`

	Network            string `help:"Network on which to listen for gRPC connections." default:"tcp"`
	Address            string `help:"Address at which to listen for gRPC connections." default:":9443"`
	TLSCertsDir        string `help:"Directory containing server certs (tls.key, tls.crt) and the CA used to verify client certificates (ca.crt)" env:"TLS_SERVER_CERTS_DIR"`
	Insecure           bool   `help:"Run without mTLS credentials. If you supply this flag --tls-server-certs-dir will be ignored."`
	MaxRecvMessageSize int    `help:"Maximum size of received messages in MB." default:"4"`
}

func (c *CLI) Run() error {
	log, err := function.NewLogger(c.Debug)
	if err != nil {
		return err
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return err
	}
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return err
	}

	return function.Serve(&Function{
		log:          log,
		k8s:          k8sClient,
		infobloxHost: os.Getenv("INFOBLOX_HOST"),
		infobloxUser: os.Getenv("INFOBLOX_USER"),
		infobloxPass: os.Getenv("INFOBLOX_PASSWORD"),
		infobloxCA:   os.Getenv("INFOBLOX_CA_CERT_PATH"),
	},
		function.Listen(c.Network, c.Address),
		function.MTLSCertificates(c.TLSCertsDir),
		function.Insecure(c.Insecure),
		function.MaxRecvMessageSize(c.MaxRecvMessageSize*1024*1024))
}

func main() {
	ctx := kong.Parse(&CLI{}, kong.Description("IP Inject — allocates IP from Infoblox WAPI and injects it into VM cloud-init."))
	ctx.FatalIfErrorf(ctx.Run())
}
