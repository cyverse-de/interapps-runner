// road-runner
//
// Executes jobs based on a JSON blob serialized to a file.
// Each step of the job runs inside a Docker container. Job results are
// transferred back into iRODS with the porklock tool. Job status updates are
// posted to the **jobs.updates** topic in the **jobs** exchange.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"

	yaml "gopkg.in/yaml.v2"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/version"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/streadway/amqp"
	"gopkg.in/cyverse-de/messaging.v4"
	"gopkg.in/cyverse-de/model.v3"

	"github.com/spf13/viper"
)

const (
	// ConfigDockerComposePathKey is the key for the config setting with the path
	// to the docker-compose executable.
	ConfigDockerComposePathKey = "docker-compose.path"

	// ConfigDockerPathKey is the key for the config setting with the path to the
	// docker executable.
	ConfigDockerPathKey = "docker.path"

	// ConfigDockerCFGKey is the key for the config setting with the path to the
	// docker client configuration file.
	ConfigDockerCFGKey = "docker.cfg"

	// ConfigProxyLowerKey is the key for the config setting with the lower bound
	// in the range from which an interactive app can be assigned a port.
	ConfigProxyLowerKey = "proxy.lower"

	// ConfigProxyUpperKey is the key for the config setting with the upper bound
	// in the range from which an interactive app can be assigned a port.
	ConfigProxyUpperKey = "proxy.upper"

	// ConfigSetfaclPathKey is the key for the config setting with the path to the
	// setfacl executable.
	ConfigSetfaclPathKey = "setfacl.path"

	// ConfigMaxCPUCoresKey is the key for the config setting with the maximum
	// number of cores that a container can use.
	ConfigMaxCPUCoresKey = "resources.max-cpu-cores"

	// ConfigMemoryLimitKey is the key for the config setting with the RAM limit
	// for each container.
	ConfigMemoryLimitKey = "resources.memory-limit"

	// ConfigFrontendBaseKey is the key for the frontend base URL configuration
	// setting.
	ConfigFrontendBaseKey = "k8s.frontend.base"

	// ConfigAppExposerBaseKey is the key for the app-exposer base URL configuration
	// setting.
	ConfigAppExposerBaseKey = "k8s.app-exposer.base"

	// ConfigHostHeaderKey is the key for the app-exposer Host header configuration
	// setting.
	ConfigHostHeaderKey = "k8s.app-exposer.header"

	// ConfigVaultURLKey is the key for the Vault URL configuration setting.
	ConfigVaultURLKey = "vault.url"

	// ConfigVaultTokenKey is the key for the Vault token configuration setting.
	ConfigVaultTokenKey = "vault.token"

	// ConfigPorklockImageKey is the key for the porklock image configuration
	// setting.
	ConfigPorklockImageKey = "porklock.image"

	// ConfigPorklockTagKey is the key for the porklock image tag configuration
	// setting.
	ConfigPorklockTagKey = "porklock.tag"

	// ConfigAMQPURIKey is the key for the AMQP URI configuration setting.
	ConfigAMQPURIKey = "amqp.uri"

	// ConfigAMQPExchangeNameKey is the key for the AMQP Exchange Name configuration
	// setting.
	ConfigAMQPExchangeNameKey = "amqp.exchange.name"

	// ConfigAMQPExchangeTypeKey is the key for the AMQP Exchange Type configuration
	// setting.
	ConfigAMQPExchangeTypeKey = "amqp.exchange.type"

	// ConfigProxyTagKey is the key for the cas-proxy image tag.
	ConfigProxyTagKey = "interapps.proxy.tag"

	// ConfigAccessHeaderKey is the key for the check-resource-access header
	// configuration setting.
	ConfigAccessHeaderKey = "k8s.check-resource-access.header"

	// ConfigAnalysisHeaderKey is the key for the get-analysis-id header
	// configuration setting.
	ConfigAnalysisHeaderKey = "k8s.get-analysis-id.header"
)

var (
	job              *model.Job
	client           *messaging.Client
	amqpExchangeName string
	amqpExchangeType string
)

var log = logrus.WithFields(logrus.Fields{
	"service": "interapps-runner",
	"art-id":  "interapps-runner",
	"group":   "org.cyverse",
})

func init() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

// Creates the output upload exclusions file, required by the JobCompose InitFromJob method.
func createUploadExclusionsFile() {
	excludeFile, err := os.Create(UploadExcludesFilename)
	if err != nil {
		log.Fatal(err)
	}
	defer excludeFile.Close()

	for _, excludePath := range job.ExcludeArguments() {
		_, err = fmt.Fprintln(excludeFile, excludePath)
		if err != nil {
			log.Fatal(err)
		}
	}
}

// CleanableJob is a job definition that contains extra information that allows
// external tools to clean up after a job.
type CleanableJob struct {
	model.Job
	LocalWorkingDir string `json:"local_working_directory"`
}

func validateInteractive(job *model.Job, tag string) error {
	// Make sure at least one step is marked as interactive.
	foundInteractive := false
	for stepIndex, s := range job.Steps {
		if s.Component.IsInteractive {
			foundInteractive = true

			if s.Component.Container.InteractiveApps.ProxyImage == "" {
				job.Steps[stepIndex].Component.Container.InteractiveApps.ProxyImage = fmt.Sprintf("discoenv/cas-proxy:%s", tag)
			}

			if s.Component.Container.InteractiveApps.ProxyName == "" {
				job.Steps[stepIndex].Component.Container.InteractiveApps.ProxyName = fmt.Sprintf("cas-proxy-%s", job.InvocationID)
			}
		}
	}
	if !foundInteractive {
		return errors.New("no interactive steps found in the job")
	}

	// Only support a single job step for now. This restriction will go away in
	// the future.
	if len(job.Steps) > 1 {
		return errors.New("interactive apps only support single tool apps for now")
	}

	return nil
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "Print the version information")
		jobFile     = flag.String("job", "", "The path to the job description file")
		cfgPath     = flag.String("config", "", "The path to the config file")
		writeTo     = flag.String("write-to", "/opt/image-janitor", "The directory to copy job files to.")
		composePath = flag.String("docker-compose", "docker-compose.yml", "The filepath to use when writing the docker-compose file.")
		composeBin  = flag.String("docker-compose-path", "/usr/bin/docker-compose", "The path to the docker-compose binary.")
		dockerBin   = flag.String("docker-path", "/usr/bin/docker", "The path to the docker binary.")
		dockerCfg   = flag.String("docker-cfg", "/var/lib/condor/.docker", "The path to the .docker directory.")
		logdriver   = flag.String("log-driver", "de-logging", "The name of the Docker log driver to use in job steps.")
		pathprefix  = flag.String("path-prefix", "/var/lib/condor", "The path prefix for the stderr/stdout logs.")
		proxyUpper  = flag.Int("proxy-upper-bound", 31399, "Upper bound in port numbers that the proxy may be reached through.")
		proxyLower  = flag.Int("proxy-lower-bound", 31300, "Lower bound in port numbers that the proxy may be reached through.")
		exposerURL  = flag.String("exposer-url", "", "The base URL to the app-exposer service.")
		exposerHost = flag.String("exposer-host-header", "", "The value of the Host header in requests to the app-exposer API.")
		setfaclPath = flag.String("setfacl-path", "/usr/bin/setfacl", "The path to the setfacl binary")
		maxCPUCores = flag.Float64("cpus", 0.0, "The default maximum amount of CPU cores that a tool can access.")
		memoryLimit = flag.Int64("memory-limit", 0, "The default maximum amount of RAM (in bytes) that a tool can access.")
		err         error
		cfg         *viper.Viper
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigquitter := make(chan bool)
	sighandler := InitSignalHandler()
	sighandler.Receive(
		sigquitter,
		func(sig os.Signal) {
			cancel() //Kill all subprocesses immediately.

			log.Info("Received signal:", sig)
			if job == nil {
				log.Warn("Info didn't get parsed from the job file, can't clean up. Probably don't need to.")
			}
			if client != nil && job != nil {
				fail(client, job, fmt.Sprintf("Received signal %s", sig))
			}
		},
		func() {
			log.Info("Signal handler is quitting")
		},
	)
	signal.Notify(
		sighandler.Signals,
		os.Interrupt,
		os.Kill,
		syscall.SIGTERM,
		syscall.SIGSTOP,
		syscall.SIGQUIT,
	)

	flag.Parse()

	if *showVersion {
		version.AppVersion()
		os.Exit(0)
	}

	if *cfgPath == "" {
		log.Fatal("--config must be set.")
	}

	log.Infof("Reading config from %s\n", *cfgPath)
	if _, err = os.Open(*cfgPath); err != nil {
		log.Fatal(*cfgPath)
	}

	cfg, err = configurate.Init(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("Done reading config from %s\n", *cfgPath)

	if *jobFile == "" {
		log.Fatal("--job must be set.")
	}

	cfg.Set(ConfigDockerComposePathKey, *composeBin)
	cfg.Set(ConfigDockerPathKey, *dockerBin)
	cfg.Set(ConfigDockerCFGKey, *dockerCfg)
	cfg.Set(ConfigProxyLowerKey, *proxyLower)
	cfg.Set(ConfigProxyUpperKey, *proxyUpper)
	cfg.Set(ConfigSetfaclPathKey, *setfaclPath)
	cfg.Set(ConfigMaxCPUCoresKey, *maxCPUCores)
	cfg.Set(ConfigMemoryLimitKey, *memoryLimit)

	if cfg.GetString(ConfigFrontendBaseKey) == "" {
		log.Fatalf("%s must be set in the configuration file", ConfigFrontendBaseKey)
	}

	if cfg.GetString(ConfigAppExposerBaseKey) == "" && *exposerURL == "" {
		log.Fatal("the exposer url must be set either in the config file or with --exposer-url")
	}

	if cfg.GetString(ConfigProxyTagKey) == "" {
		log.Fatal("the interapps.proxy.tag must be set in the config")
	}

	// prefer the command-line setting over the config setting.
	if *exposerURL != "" {
		cfg.Set(ConfigAppExposerBaseKey, *exposerURL)
	}

	if *exposerHost != "" {
		cfg.Set(ConfigHostHeaderKey, *exposerHost)
	} else {
		if cfg.GetString(ConfigHostHeaderKey) == "" {
			cfg.Set(ConfigHostHeaderKey, "app-exposer")
		}
	}

	if cfg.GetString(ConfigAnalysisHeaderKey) == "" {
		log.Fatal("the k8s.get-analysis-id.header must be set in the config")
	}

	if cfg.GetString(ConfigAccessHeaderKey) == "" {
		log.Fatal("the k8s.check-resource-access.header must be set in the config")
	}

	if *memoryLimit != 0 {
		cfg.Set(ConfigMemoryLimitKey, *memoryLimit)
	} else {
		if cfg.GetInt64(ConfigMemoryLimitKey) == 0 {
			cfg.Set(ConfigMemoryLimitKey, 4000000000)
		}
	}

	if *maxCPUCores != 0.0 {
		cfg.Set(ConfigMaxCPUCoresKey, *maxCPUCores)
	} else {
		if cfg.GetFloat64(ConfigMaxCPUCoresKey) == 0.0 {
			cfg.Set(ConfigMaxCPUCoresKey, 2.0)
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	// Read in the job definition from the path passed in on the command-line
	data, err := ioutil.ReadFile(*jobFile)
	if err != nil {
		log.Fatal(err)
	}

	// Intialize a job model from the data read from the job definition.
	job, err = model.NewFromData(cfg, data)
	if err != nil {
		log.Fatal(err)
	}

	// Make sure that the job contains enough information to allow an interactive
	// job to run successfully.
	if err = validateInteractive(job, cfg.GetString(ConfigProxyTagKey)); err != nil {
		log.Fatal(err)
	}

	// Create a cleanable version of the job. Adds a bit more data to allow
	// image-janitor and network-pruner to do their work.
	cleanable := &CleanableJob{*job, wd}

	cleanablejson, err := json.Marshal(cleanable)
	if err != nil {
		log.Fatal(errors.Wrap(err, "failed to marshal json for job cleaning"))
	}

	// Check for the existence of the path at *writeTo
	if _, err = os.Open(*writeTo); err != nil {
		log.Fatal(err)
	}

	// Write out the cleanable job JSON to the *writeTo directory. This will be
	// where network-pruner and image-janitor read the job data from.
	if err = WriteJob(FS, job.InvocationID, *writeTo, cleanablejson); err != nil {
		log.Fatal(err)
	}

	// Configure and initialize the AMQP connection. It will be used to listen for
	// stop requests and send out job status notifications.
	uri := cfg.GetString(ConfigAMQPURIKey)
	amqpExchangeName = cfg.GetString(ConfigAMQPExchangeNameKey)
	amqpExchangeType = cfg.GetString(ConfigAMQPExchangeTypeKey)
	client, err = messaging.NewClient(uri, true)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Configured the AMQP client so we can publish messages such as the job
	// status updates.
	client.SetupPublishing(amqpExchangeName)

	availablePort, err := AvailableTCPPort(*proxyLower, *proxyUpper)
	if err != nil {
		log.Fatal(err)
	}

	// Generate the docker-compose file used to execute the job.
	composer, err := NewComposer(job, cfg, *logdriver, *pathprefix)
	if err != nil {
		log.Fatal(err)
	}

	// Create the output upload exclusions file required by the JobCompose InitFromJob method.
	createUploadExclusionsFile()

	// Populates the data structure that will become the docker-compose file with
	// information from the job definition.
	composer.InitFromJob(wd, availablePort)

	// Write out the docker-compose file. This will get transferred back with the
	// job outputs, which makes debugging stuff a lot easier.
	c, err := os.Create(*composePath)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	m, err := yaml.Marshal(composer.composition)
	if err != nil {
		log.Fatal(err)
	}
	_, err = c.Write(m)
	if err != nil {
		log.Fatal(err)
	}
	c.Close()

	// The channel that the exit code will be passed along on.
	exit := make(chan messaging.StatusCode)

	// Launch the go routine that will handle job exits by signal or timer.
	go Exit(cancel, exit)

	// Listen for stop requests. Make sure Listen() is called before the stop
	// request message consumer is added, otherwise there's a race condition that
	// might cause stop requests to disappear into the void.
	go client.Listen()

	client.AddDeletableConsumer(
		amqpExchangeName,
		amqpExchangeType,
		messaging.StopQueueName(job.InvocationID),
		messaging.StopRequestKey(job.InvocationID),
		func(d amqp.Delivery) {
			d.Ack(false)
			running(client, job, "Received stop request")
			exit <- messaging.StatusKilled
		},
	)

	// Actually execute all of the job steps.
	exitCode := Run(ctx, client, cfg, composer, exit, availablePort)

	// Clean up the job file. Cleaning it out will prevent image-janitor and
	// network-pruner from continuously trying to clean up after the job.
	if err = DeleteJobFile(FS, job.InvocationID, *writeTo); err != nil {
		log.Errorf("%+v", err)
	}

	cancel() //again, just in case.

	// Exit with the status code of the job.
	os.Exit(int(exitCode))
}
