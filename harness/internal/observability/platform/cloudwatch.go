package platform

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
)

// cloudwatchDefinition sends all three signals to the Amazon CloudWatch
// OTLP endpoints, signed with SigV4 from the AWS default credential chain.
//
// Options: region (else the AWS chain's region), logGroup (required) and
// logStream (default cloudwatchDefaultLogStream). The logs endpoint does
// not create the group or stream; both must already exist.
var cloudwatchDefinition = definition{
	options: map[string]bool{"region": false, "logGroup": true, "logStream": false},
	build:   buildCloudWatch,
}

const cloudwatchDefaultLogStream = "default"

// credentialCheckTimeout bounds the credential retrieval made while the
// profile resolves.
const credentialCheckTimeout = 5 * time.Second

// cloudwatchSigningServices is the SigV4 service name each signal's
// endpoint verifies signatures against.
var cloudwatchSigningServices = map[otlp.Signal]string{
	otlp.SignalTraces:  "xray",
	otlp.SignalMetrics: "monitoring",
	otlp.SignalLogs:    "logs",
}

var (
	awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-[0-9]+$`)
	// Regions in partitions whose endpoints are not under amazonaws.com:
	// China, the ISO partitions and the European Sovereign Cloud.
	awsOtherPartitionRegionPattern = regexp.MustCompile(`^(cn|eusc)-|-iso[a-z]?-`)
	// CloudWatch Logs naming rules for log groups and log streams.
	cloudwatchLogGroupPattern  = regexp.MustCompile(`^[A-Za-z0-9._/#-]{1,512}$`)
	cloudwatchLogStreamPattern = regexp.MustCompile(`^[^:*]{1,512}$`)
)

func buildCloudWatch(ctx context.Context, opts map[string]string, deps Deps) (Profile, error) {
	logGroup := opts["logGroup"]
	if !cloudwatchLogGroupPattern.MatchString(logGroup) {
		return Profile{}, fmt.Errorf("logGroup %q must match %s", logGroup, cloudwatchLogGroupPattern)
	}
	logStream := firstNonEmpty(opts["logStream"], cloudwatchDefaultLogStream)
	if !cloudwatchLogStreamPattern.MatchString(logStream) {
		return Profile{}, fmt.Errorf("logStream %q must not contain ':' or '*'", logStream)
	}

	load := deps.LoadAWSConfig
	if load == nil {
		load = loadDefaultAWSConfig
	}
	awsCfg, err := load(ctx, opts["region"])
	if err != nil {
		return Profile{}, fmt.Errorf("load AWS config: %w", err)
	}
	region := firstNonEmpty(opts["region"], awsCfg.Region)
	if region == "" {
		return Profile{}, fmt.Errorf("no AWS region: set platformOptions region or configure one for the AWS SDK (AWS_REGION or the shared config profile)")
	}
	if awsOtherPartitionRegionPattern.MatchString(region) {
		return Profile{}, fmt.Errorf("region %q is outside the aws and aws-us-gov partitions, whose *.amazonaws.com endpoints are the only ones supported", region)
	}
	if !awsRegionPattern.MatchString(region) {
		return Profile{}, fmt.Errorf("region %q is not an AWS region name", region)
	}
	if awsCfg.Credentials == nil {
		return Profile{}, fmt.Errorf("the AWS default credential chain provided no credentials provider")
	}
	// The chain resolves lazily, so retrieving once here turns missing
	// credentials into a resolution error instead of failed exports.
	retrieveCtx, cancel := context.WithTimeout(ctx, credentialCheckTimeout)
	defer cancel()
	if _, err := awsCfg.Credentials.Retrieve(retrieveCtx); err != nil {
		return Profile{}, credentialsError(err)
	}

	signer := v4.NewSigner()
	return Profile{
		RequiredProtocol: otlp.ProtocolHTTPProtobuf,
		Signals: map[otlp.Signal]SignalDefaults{
			otlp.SignalTraces:  {Endpoint: cloudwatchEndpoint("xray", region)},
			otlp.SignalMetrics: {Endpoint: cloudwatchEndpoint("monitoring", region)},
			otlp.SignalLogs: {
				Endpoint: cloudwatchEndpoint("logs", region),
				Headers: map[string]string{
					"x-aws-log-group":  logGroup,
					"x-aws-log-stream": logStream,
				},
			},
		},
		// The resource attributes the AWS Distro for OpenTelemetry sets
		// for agent observability.
		ResourceAttrs: []attribute.KeyValue{
			attribute.String("aws.service.type", "gen_ai_agent"),
			attribute.String("aws.log.group.names", logGroup),
		},
		WrapTransport: func(sig otlp.Signal, base http.RoundTripper) http.RoundTripper {
			return &sigV4Transport{
				base:        base,
				credentials: awsCfg.Credentials,
				signer:      signer,
				service:     cloudwatchSigningServices[sig],
				region:      region,
				now:         time.Now,
			}
		},
	}, nil
}

// cloudwatchEndpoint returns the OTLP base URL of one CloudWatch service;
// the exporter appends "/v1/<signal>".
func cloudwatchEndpoint(service, region string) string {
	return "https://" + service + "." + region + ".amazonaws.com"
}

func loadDefaultAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}
