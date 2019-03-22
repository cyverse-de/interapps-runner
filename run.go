package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/kr/pty"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"gopkg.in/cyverse-de/messaging.v6"
	"gopkg.in/cyverse-de/model.v4"
)

// logrusProxyWriter will prevent
// "Error while reading from Writer: bufio.Scanner: token too long" errors
// if a docker command generates a lot of output
// (from pulling many input containers at once, for example)
// and Logrus attempts to log all of that output in one log line.
type logrusProxyWriter struct {
	entry *logrus.Entry
}

func (w *logrusProxyWriter) Write(b []byte) (int, error) {
	return fmt.Fprintf(w.entry.Writer(), string(b))
}

var logWriter = &logrusProxyWriter{
	entry: log,
}

// JobRunner provides the functionality needed to run jobs.
type JobRunner struct {
	client            JobUpdatePublisher
	exit              chan messaging.StatusCode
	composer          *Composer
	status            messaging.StatusCode
	logsDir           string
	volumeDir         string
	workingDir        string
	projectName       string
	tmpDir            string
	networkName       string
	availablePort     int
	dockerPath        string
	dockerComposePath string
	vaultURL          string
	vaultToken        string
	setfaclPath       string
	appExposerBaseURL string
	appExposerHeader  string
}

// NewJobRunner creates a new JobRunner
func NewJobRunner(client JobUpdatePublisher, cfg *viper.Viper, c *Composer, exit chan messaging.StatusCode, availablePort int) (*JobRunner, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	runner := &JobRunner{
		client:            client,
		exit:              exit,
		composer:          c,
		status:            messaging.Success,
		workingDir:        cwd,
		volumeDir:         path.Join(cwd, VOLUMEDIR),
		logsDir:           path.Join(cwd, VOLUMEDIR, "logs"),
		tmpDir:            path.Join(cwd, TMPDIR),
		availablePort:     availablePort,
		dockerPath:        cfg.GetString(ConfigDockerPathKey),
		dockerComposePath: cfg.GetString(ConfigDockerComposePathKey),
		vaultURL:          cfg.GetString(ConfigVaultURLKey),
		vaultToken:        cfg.GetString(ConfigVaultTokenKey),
		setfaclPath:       cfg.GetString(ConfigSetfaclPathKey),
		appExposerBaseURL: cfg.GetString(ConfigAppExposerBaseKey),
		appExposerHeader:  cfg.GetString(ConfigHostHeaderKey),
	}
	return runner, nil
}

// Init will initialize the state for a JobRunner. The volumeDir and logsDir
// will get created.
func (r *JobRunner) Init(ctx context.Context) error {
	err := os.MkdirAll(r.logsDir, 0755)
	if err != nil {
		return err
	}

	// Set the default ACLs on the working directory volume so that the current
	// user will be able to manage files within it regardless of the uid of any
	// containers that operate on files in it. This will allow us to further
	// modify ACLs on a per-step basis so that the container user will be able to
	// use files created by other containers.
	uid := os.Getuid()
	if err = r.AddWorkingVolumeACL(ctx, uid); err != nil {
		return err
	}

	for _, step := range r.composer.job.Steps {
		// If the UID for the container is set, then we need to give permissions to
		// it here so that the user can access files in the image. If it's not set,
		// it will default to 0 which is effectively the default user for Docker
		// anyway, so it won't make a difference here.
		imgUID := step.Component.Container.UID

		// This should enable the user recorded in the image to access the files in
		// the bind mounted working volume. We have a default ACL that lets the user
		// interapps-runner is running as access the files as well.
		if err = r.AddWorkingVolumeACL(ctx, imgUID); err != nil {
			return err
		}
	}

	err = os.MkdirAll(r.tmpDir, 0755)
	if err != nil {
		return err
	}

	// Set world-write perms on volumeDir, so non-root users can create job outputs.
	err = os.Chmod(r.volumeDir, 0777)
	if err != nil {
		// Log error and continue.
		log.Error(err)
	}

	// Copy docker-compose file to the log dir for debugging purposes.
	err = CopyFile(FS, "docker-compose.yml", path.Join(r.logsDir, "docker-compose.yml"))
	if err != nil {
		// Log error and continue.
		log.Error(err)
	}

	// Copy upload exclude list to the log dir for debugging purposes.
	err = CopyFile(FS, UploadExcludesFilename, path.Join(r.logsDir, UploadExcludesFilename))
	if err != nil {
		// Log error and continue.
		log.Error(err)
	}

	transferTrigger, err := os.Create(path.Join(r.logsDir, "de-transfer-trigger.log"))
	if err != nil {
		return err
	}
	defer transferTrigger.Close()
	_, err = transferTrigger.WriteString("This is only used to force HTCondor to transfer files.")
	if err != nil {
		return err
	}

	if _, err = os.Stat("iplant.cmd"); err != nil {
		if err = os.Rename("iplant.cmd", path.Join(r.logsDir, "iplant.cmd")); err != nil {
			return err
		}
	}

	return nil
}

// DockerLogin will run "docker login" with credentials sent with the job.
func (r *JobRunner) DockerLogin() error {
	var err error

	// Login so that images can be pulled.
	var authinfo *authInfo
	for _, img := range r.composer.job.ContainerImages() {
		if img.Auth != "" {
			authinfo, err = parse(img.Auth)
			if err != nil {
				return err
			}
			authCommand := exec.Command(
				r.dockerPath,
				"login",
				"--username",
				authinfo.Username,
				"--password",
				authinfo.Password,
				parseRepo(img.Name),
			)
			f, err := pty.Start(authCommand)
			if err != nil {
				return err
			}
			go func() {
				io.Copy(logWriter, f)
			}()
			err = authCommand.Wait()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// JobUpdatePublisher is the interface for types that need to publish a job
// update.
type JobUpdatePublisher interface {
	PublishJobUpdate(m *messaging.UpdateMessage) error
}

func (r *JobRunner) execDockerCompose(ctx context.Context, svcname string, env []string, stdout, stderr io.Writer) error {
	var err error
	cmd := exec.CommandContext(
		ctx,
		r.dockerComposePath,
		"-p",
		r.projectName,
		"-f",
		"docker-compose.yml",
		"up",
		"--abort-on-container-exit",
		"--exit-code-from", svcname,
		"--no-color",
		svcname,
	)
	cmd.Env = env
	cmd.Stderr = stdout
	cmd.Stdout = stderr
	if err = cmd.Run(); err != nil {
		return err
	}
	return nil
}

func (r *JobRunner) createDataContainers(ctx context.Context) (messaging.StatusCode, error) {
	var err error

	for index := range r.composer.job.DataContainers() {
		svcname := fmt.Sprintf("data_%d", index)

		running(r.client, r.composer.job, fmt.Sprintf("creating data container data_%d", index))

		if err = r.execDockerCompose(ctx, svcname, os.Environ(), logWriter, logWriter); err != nil {
			running(
				r.client,
				r.composer.job,
				fmt.Sprintf("error creating data container data_%d: %s", index, err.Error()),
			)
			return messaging.StatusDockerCreateFailed, errors.Wrapf(err, "failed to create data container data_%d", index)
		}

		running(r.client, r.composer.job, fmt.Sprintf("finished creating data container data_%d", index))
	}

	return messaging.Success, nil
}

func (r *JobRunner) downloadInputs(ctx context.Context) (messaging.StatusCode, error) {
	var exitCode int64

	env := os.Environ()
	env = append(env, fmt.Sprintf("VAULT_ADDR=%s", r.vaultURL))
	env = append(env, fmt.Sprintf("VAULT_TOKEN=%s", r.vaultToken))

	for index, input := range r.composer.job.Inputs() {
		running(r.client, r.composer.job, fmt.Sprintf("Downloading %s", input.IRODSPath()))

		stderr, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("logs-stderr-input-%d", index)))
		if err != nil {
			log.Error(err)
		}
		defer stderr.Close()

		stdout, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("logs-stdout-input-%d", index)))
		if err != nil {
			log.Error(err)
		}
		defer stdout.Close()

		svcname := fmt.Sprintf("input_%d", index)
		if err = r.execDockerCompose(ctx, svcname, env, stdout, stderr); err != nil {
			running(r.client, r.composer.job, fmt.Sprintf("error downloading %s: %s", input.IRODSPath(), err.Error()))
			return messaging.StatusInputFailed, errors.Wrapf(err, "failed to download %s with an exit code of %d", input.IRODSPath(), exitCode)
		}

		stdout.Close()
		stderr.Close()

		running(r.client, r.composer.job, fmt.Sprintf("finished downloading %s", input.IRODSPath()))
	}

	return messaging.Success, nil
}

// AddWorkingVolumeACL adds an ACL for the given UID to the working directory
// that gets mounted into each container that runs as part of the job. It grants
// rwx perms recursively. It is not a default ACL.
func (r *JobRunner) AddWorkingVolumeACL(ctx context.Context, uid int) error {
	log.Printf("adding rwx acl on %s recursively for uid %d", r.volumeDir, uid)
	cmd := exec.CommandContext(ctx, r.setfaclPath, "-R", "-m", fmt.Sprintf("d:u:%d:rwx", uid), r.volumeDir)
	cmd.Env = os.Environ()
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	return cmd.Run()
}

// RemoveWorkingVolumeACL removes an ACL for the given UID from the working
// directory that gets mounted into each container that runs as part of the job.
func (r *JobRunner) RemoveWorkingVolumeACL(ctx context.Context, uid int) error {
	log.Printf("removing rwx acl on %s recursively for uid %d", r.volumeDir, uid)
	cmd := exec.CommandContext(ctx, r.setfaclPath, "-R", "-x", fmt.Sprintf("u:%d", uid), r.volumeDir)
	cmd.Env = os.Environ()
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	return cmd.Run()
}

type authInfo struct {
	Username string
	Password string
}

func parse(b64 string) (*authInfo, error) {
	jsonstring, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	a := &authInfo{}
	err = json.Unmarshal(jsonstring, a)
	return a, err
}

func (r *JobRunner) websocketURL(step *model.Step, backendURL string) (string, error) {
	var websocketURL string
	if step.Component.Container.InteractiveApps.WebsocketPath != "" ||
		step.Component.Container.InteractiveApps.WebsocketProto != "" ||
		step.Component.Container.InteractiveApps.WebsocketPort != "" {

		burl, err := url.Parse(backendURL)
		if err != nil {
			return "", errors.Wrapf(err, "couldn't parse URL %s", backendURL)
		}

		var wsPath, wsProto, wsPort string
		if step.Component.Container.InteractiveApps.WebsocketPath != "" {
			wsPath = step.Component.Container.InteractiveApps.WebsocketPath
		} else {
			wsPath = burl.Path
		}

		if step.Component.Container.InteractiveApps.WebsocketProto != "" {
			wsProto = step.Component.Container.InteractiveApps.WebsocketProto
		} else {
			wsProto = burl.Scheme
		}

		if step.Component.Container.InteractiveApps.WebsocketPort != "" {
			wsPort = step.Component.Container.InteractiveApps.WebsocketPort
		} else {
			wsPort = burl.Port()
		}

		burl.Path = wsPath
		burl.Scheme = wsProto
		if wsPort != "" {
			burl.Host = fmt.Sprintf("%s:%s", burl.Hostname(), wsPort)
		}
		websocketURL = burl.String()
	}
	return websocketURL, nil
}

type asyncReturn struct {
	statusCode messaging.StatusCode
	err        error
}

func (r *JobRunner) runAllSteps(parent context.Context) (messaging.StatusCode, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	for idx, step := range r.composer.job.Steps {
		stepStatus := messaging.Success
		var stepErr error

		running(r.client, r.composer.job,
			fmt.Sprintf(
				"Running tool container %s:%s with arguments: %s",
				step.Component.Container.Image.Name,
				step.Component.Container.Image.Tag,
				strings.Join(step.Arguments(), " "),
			),
		)

		stdout, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("docker-compose-step-stdout-%d", idx)))
		if err != nil {
			log.Error(err)
		}
		defer stdout.Close()

		stderr, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("docker-compose-step-stderr-%d", idx)))
		if err != nil {
			log.Error(err)
		}
		defer stderr.Close()

		proxystdout, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("docker-compose-step-proxy-stdout-%d", idx)))
		if err != nil {
			log.Error(err)
		}
		defer proxystdout.Close()

		proxystderr, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("docker-compose-step-proxy-stderr-%d", idx)))
		if err != nil {
			log.Error(err)
		}
		defer proxystderr.Close()

		go func() {
			if err = r.execDockerCompose(ctx, ProxyServiceName(idx), os.Environ(), proxystdout, proxystderr); err != nil {
				running(r.client, r.composer.job, fmt.Sprintf("error running proxy %s", err.Error()))
			}
		}()

		ingressID := r.composer.IngressID()

		// Used to get the messaging status code and errors from the ConfigureK8s
		// function.
		configChan := make(chan asyncReturn)

		// Call the ConfigureK8s function in a goroutine. It's cancelable, so pass
		// in the context.
		go func(ctx context.Context, c chan asyncReturn) {
			code, k8serr := r.ConfigureK8s(ctx, ingressID)

			// Ship the status code and err back to the calling goroutine.
			c <- asyncReturn{
				statusCode: code,
				err:        k8serr,
			}
		}(ctx, configChan)

		// Used to get the status code and error from the step execution.
		execChan := make(chan asyncReturn)

		go func(ctx context.Context, c chan asyncReturn) {
			svcname := fmt.Sprintf("step_%d", idx)
			if err = r.execDockerCompose(ctx, svcname, os.Environ(), stdout, stderr); err != nil {
				running(r.client, r.composer.job,
					fmt.Sprintf(
						"Error running tool container %s:%s with arguments '%s': %s",
						step.Component.Container.Image.Name,
						step.Component.Container.Image.Tag,
						strings.Join(step.Arguments(), " "),
						err.Error(),
					),
				)

				c <- asyncReturn{
					statusCode: messaging.StatusStepFailed,
					err:        err,
				}
				return
			}

			running(r.client, r.composer.job,
				fmt.Sprintf("Tool container %s:%s with arguments '%s' finished successfully",
					step.Component.Container.Image.Name,
					step.Component.Container.Image.Tag,
					strings.Join(step.Arguments(), " "),
				),
			)
			c <- asyncReturn{
				statusCode: messaging.Success,
				err:        nil,
			}
		}(ctx, execChan)

		shouldExit := false
		for !shouldExit {
			select {
			case execReturn := <-execChan: // step exection is done
				stepStatus = execReturn.statusCode
				stepErr = execReturn.err
				if err != nil {
					log.Println("error from step execution, canceling contexts")
					cancel()
				}
				shouldExit = true
				break
			case configReturn := <-configChan: // k8s configuration is done
				stepStatus = configReturn.statusCode
				stepErr = configReturn.err
				if err != nil {
					log.Println("error from k8s configuration, canceling contexts")
					cancel()
					shouldExit = true
				} else {
					shouldExit = false // this will probably exit before the step
				}
				break
			case <-ctx.Done(): // The context got canceled
				stepStatus = messaging.StatusStepFailed
				stepErr = ctx.Err()
				shouldExit = true
				break
			}
		}

		// I'm not sure if ignoring the errors here (aside from logging them) is the
		// right thing to do, but it's easy to fix if it becomes a problem. Just
		// return messaging.StatusStepFailed and the error.
		log.Printf("deleting K8s endpoint %s\n", ingressID)
		if err = DeleteK8SEndpoint(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID); err != nil {
			running(r.client, r.composer.job, fmt.Sprintf("Error deleting K8s endpoint: %s", err.Error()))
		}
		log.Printf("done deleting K8s endpoint %s\n", ingressID)

		log.Printf("deleting K8s service %s\n", ingressID)
		if err = DeleteK8SService(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID); err != nil {
			running(r.client, r.composer.job, fmt.Sprintf("Error deleting K8s service: %s", err.Error()))
		}
		log.Printf("done deleting K8s service %s\n", ingressID)

		log.Printf("deleting K8s ingress %s\n", ingressID)
		if err = DeleteK8SIngress(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID); err != nil {
			running(r.client, r.composer.job, fmt.Sprintf("Error deleting K8s ingress: %s", err.Error()))
		}
		log.Printf("done deleting K8s ingress %s\n", ingressID)

		if stepErr != nil {
			return stepStatus, stepErr
		}
	}

	return messaging.Success, nil
}

// ConfigureK8s calls into the Kubernetes API to set up and Endpoint, Service,
// and Ingress entry for the job.
func (r *JobRunner) ConfigureK8s(ctx context.Context, ingressID string) (messaging.StatusCode, error) {
	var err error
	var ready, ok bool

	hostIP := GetOutboundIP()
	u := fmt.Sprintf("http://%s:%d/url-ready", hostIP, r.availablePort)

	for !ready {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return messaging.StatusStepFailed, err
		}

		err = HTTPDo(ctx, req, func(resp *http.Response, err error) error {
			if err != nil {
				log.Errorf("error checking url-ready: %s\n", err)
			} else {
				if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
					respbody, err := ioutil.ReadAll(resp.Body)
					if err != nil {
						return fmt.Errorf("error reading response body: %s", err.Error())
					}
					resp.Body.Close()

					bodymap := map[string]bool{}
					if err = json.Unmarshal(respbody, &bodymap); err != nil {
						return fmt.Errorf("error unmarshalling json: %s", err)
					}

					ready, ok = bodymap["ready"]
					if !ok {
						ready = false
					}
				}
			}
			return nil
		})

		if err != nil {
			log.Errorf("error hitting the apps url-ready endpoint: %s", err)
		}

		if !ready {
			time.Sleep(5 * time.Second)
		}
	}

	log.Printf("creating K8s endpoint %s\n", ingressID)
	eptcfg := &EndpointConfig{
		IP:   hostIP.String(),
		Name: ingressID,
		Port: r.availablePort,
	}
	if err = CreateK8SEndpoint(ctx, r.appExposerBaseURL, r.appExposerHeader, eptcfg); err != nil {
		running(r.client, r.composer.job, fmt.Sprintf("Error creating K8s Endpoint: %s", err.Error()))
		DeleteK8SEndpoint(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID)
		return messaging.StatusStepFailed, err
	}
	log.Printf("done creating K8s endpoint %s\n", ingressID)

	log.Printf("creating K8s service %s\n", ingressID)
	svccfg := &ServiceConfig{
		TargetPort: r.availablePort,
		Name:       ingressID,
		ListenPort: 80,
	}
	if err = CreateK8SService(ctx, r.appExposerBaseURL, r.appExposerHeader, svccfg); err != nil {
		running(r.client, r.composer.job, fmt.Sprintf("Error creating K8s Service: %s", err.Error()))
		DeleteK8SService(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID)
		DeleteK8SEndpoint(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID)
		return messaging.StatusStepFailed, err
	}
	log.Printf("done creating K8s service %s\n", ingressID)

	log.Printf("creating K8s ingress %s\n", ingressID)
	ingcfg := &IngressConfig{
		Service: ingressID,
		Port:    80,
		Name:    ingressID,
	}
	if err = CreateK8SIngress(ctx, r.appExposerBaseURL, r.appExposerHeader, ingcfg); err != nil {
		running(r.client, r.composer.job, fmt.Sprintf("Error creating K8s Ingress: %s", err.Error()))
		DeleteK8SIngress(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID)
		DeleteK8SService(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID)
		DeleteK8SEndpoint(ctx, r.appExposerBaseURL, r.appExposerHeader, ingressID)
		return messaging.StatusStepFailed, err
	}
	log.Printf("done creating K8s ingress %s\n", ingressID)

	return messaging.Success, err
}

func (r *JobRunner) uploadOutputs() (messaging.StatusCode, error) {
	var err error

	stdout, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("logs-stdout-output")))
	if err != nil {
		log.Error(err)
	}
	defer stdout.Close()

	stderr, err := os.Create(path.Join(r.logsDir, fmt.Sprintf("logs-stderr-output")))
	if err != nil {
		log.Error(err)
	}
	defer stderr.Close()

	env := []string{
		fmt.Sprintf("VAULT_ADDR=%s", r.vaultURL),
		fmt.Sprintf("VAULT_TOKEN=%s", r.vaultToken),
	}

	// We're using the background context so that this stuff will run even when
	// the job is cancelled.
	if err = r.execDockerCompose(context.Background(), "upload_outputs", env, stdout, stderr); err != nil {
		running(r.client, r.composer.job, fmt.Sprintf("Error uploading outputs to %s: %s", r.composer.job.OutputDirectory(), err.Error()))
		return messaging.StatusOutputFailed, errors.Wrapf(err, "failed to upload outputs to %s", r.composer.job.OutputDirectory())
	}

	running(r.client, r.composer.job, fmt.Sprintf("Done uploading outputs to %s", r.composer.job.OutputDirectory()))

	return messaging.Success, nil
}

func parseRepo(imagename string) string {
	if strings.Contains(imagename, "/") {
		parts := strings.Split(imagename, "/")
		return parts[0]
	}
	return ""
}

func (r *JobRunner) execCmd(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = os.Environ()
	cmd.Dir = r.workingDir
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	return cmd.Run()
}

func (r *JobRunner) pullProxyImage(ctx context.Context, dockerPath, proxyImg string) error {
	return r.execCmd(ctx, dockerPath, "pull", proxyImg)
}

func (r *JobRunner) createProxyContainer(ctx context.Context, dockerPath, proxyImg, containerName, networkName string) error {
	return r.execCmd(ctx, dockerPath, "create", "--name", containerName, "--network", networkName, proxyImg)
}

func (r *JobRunner) killProxyContainer(ctx context.Context, dockerPath, containerName string) error {
	return r.execCmd(ctx, dockerPath, "kill", containerName)
}

type proxyContainerConfig struct {
	dockerPath    string
	containerName string
	containerImg  string
	backendURL    string
	frontendURL   string
	websocketURL  string
	casURL        string
	casValidate   string
	sslCertPath   string
	sslKeyPath    string
	hostPort      string
}

func (r *JobRunner) runProxyContainer(ctx context.Context, cfg *proxyContainerConfig) error {
	cmdElements := []string{
		cfg.dockerPath,
		"run",
		"--rm",
		"-p", fmt.Sprintf("%s:8080", cfg.hostPort),
		"--network", r.networkName,
		"--name", cfg.containerName,
	}

	lastElements := []string{
		cfg.containerImg,
		"--backend-url", cfg.backendURL,
		"--ws-backend-url", cfg.websocketURL,
		"--frontend-url", cfg.frontendURL,
		"--cas-base-url", cfg.casURL,
		"--cas-validate", cfg.casValidate,
	}

	if cfg.sslCertPath != "" {
		cmdElements = append(cmdElements, "-v", fmt.Sprintf("%s:%s", cfg.sslCertPath, cfg.sslCertPath))
		lastElements = append(lastElements, "--ssl-cert", cfg.sslCertPath)
	}

	if cfg.sslKeyPath != "" {
		cmdElements = append(cmdElements, "-v", fmt.Sprintf("%s:%s", cfg.sslKeyPath, cfg.sslKeyPath))
		lastElements = append(lastElements, "--ssl-key", cfg.sslKeyPath)
	}

	return r.execCmd(ctx, append(cmdElements, lastElements...)...)
}

func (r *JobRunner) createNetwork(ctx context.Context, dockerPath, networkName string) error {
	return r.execCmd(ctx, dockerPath, "network", "create", "--driver", "bridge", networkName)
}

func (r *JobRunner) dockerComposePull(ctx context.Context, composePath string) error {
	return r.execCmd(ctx, composePath, "-p", r.projectName, "-f", "docker-compose.yml", "pull", "--parallel")

}

// Run executes the job, and returns the exit code on the exit channel.
func Run(ctx context.Context, client JobUpdatePublisher, cfg *viper.Viper, c *Composer, exit chan messaging.StatusCode, availablePort int) messaging.StatusCode {
	host, err := os.Hostname()
	if err != nil {
		log.Error(err)
		host = "UNKNOWN"
	}

	runner, err := NewJobRunner(client, cfg, c, exit, availablePort)
	if err != nil {
		log.Error(err)
	}

	err = runner.Init(ctx)
	if err != nil {
		log.Error(err)
	}

	runner.projectName = strings.Replace(runner.composer.job.InvocationID, "-", "", -1)
	runner.networkName = fmt.Sprintf("%s_default", runner.projectName)

	// let everyone know the job is running
	running(runner.client, runner.composer.job, fmt.Sprintf("Job %s is running on host %s", runner.composer.job.InvocationID, host))

	if err = runner.DockerLogin(); err != nil {
		log.Error(err)
	}

	if err = runner.createNetwork(ctx, runner.dockerPath, runner.networkName); err != nil {
		log.Error(err) // don't need to fail, since docker-compose is *supposed* to create the network
	}

	if err = runner.dockerComposePull(ctx, runner.dockerComposePath); err != nil {
		log.Error(err)
		runner.status = messaging.StatusDockerPullFailed
	}

	if err = WriteJobSummary(FS, runner.logsDir, job); err != nil {
		log.Error(err)
	}

	if err = WriteJobParameters(FS, runner.logsDir, job); err != nil {
		log.Error(err)
	}

	if runner.status == messaging.Success {
		if runner.status, err = runner.createDataContainers(ctx); err != nil {
			log.Error(err)
		}
	}

	// If pulls didn't succeed then we can't guarantee that we've got the
	// correct versions of the tools. Don't bother pulling in data in that case,
	// things are already screwed up.
	if runner.status == messaging.Success {
		if runner.status, err = runner.downloadInputs(ctx); err != nil {
			log.Error(err)
		}
	}

	// Only attempt to run the steps if the input downloads succeeded. No reason
	// to run the steps if there's no/corrupted data to operate on.
	if runner.status == messaging.Success {
		if runner.status, err = runner.runAllSteps(ctx); err != nil {
			log.Error(err)
		}
	}

	// Always attempt to transfer outputs. There might be logs that can help
	// debug issues when the job fails.
	var outputStatus messaging.StatusCode
	running(runner.client, runner.composer.job, fmt.Sprintf("Beginning to upload outputs to %s", runner.composer.job.OutputDirectory()))
	if outputStatus, err = runner.uploadOutputs(); err != nil {
		log.Error(err)
	}
	if outputStatus != messaging.Success {
		runner.status = outputStatus
	}

	// Always inform upstream of the job status.
	if runner.status != messaging.Success {
		fail(runner.client, runner.composer.job, fmt.Sprintf("Job exited with a status of %d", runner.status))
	} else {
		success(runner.client, runner.composer.job)
	}

	// Clean up, you filthy animal
	downCommand := exec.Command(runner.dockerComposePath, "-p", runner.projectName, "-f", "docker-compose.yml", "down", "-v")
	downCommand.Stderr = log.Writer()
	downCommand.Stdout = log.Writer()
	if err = downCommand.Run(); err != nil {
		log.Errorf("%+v\n", err)
	}

	netCmd := exec.Command(runner.dockerPath, "network", "rm", fmt.Sprintf("%s_default", runner.projectName))
	netCmd.Stderr = log.Writer()
	netCmd.Stdout = log.Writer()
	if err = netCmd.Run(); err != nil {
		log.Errorf("%+v\n", err)
	}

	return runner.status
}
