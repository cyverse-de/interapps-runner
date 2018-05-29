package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/cyverse-de/interapps-runner/dcompose"
	"github.com/cyverse-de/interapps-runner/fs"
	"github.com/kr/pty"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"gopkg.in/cyverse-de/messaging.v4"
	"gopkg.in/cyverse-de/model.v2"
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
	client        JobUpdatePublisher
	exit          chan messaging.StatusCode
	job           *model.Job
	status        messaging.StatusCode
	cfg           *viper.Viper
	logsDir       string
	volumeDir     string
	workingDir    string
	projectName   string
	tmpDir        string
	networkName   string
	availablePort int
}

// NewJobRunner creates a new JobRunner
func NewJobRunner(client JobUpdatePublisher, job *model.Job, cfg *viper.Viper, exit chan messaging.StatusCode, availablePort int) (*JobRunner, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	runner := &JobRunner{
		client:        client,
		exit:          exit,
		job:           job,
		cfg:           cfg,
		status:        messaging.Success,
		workingDir:    cwd,
		volumeDir:     path.Join(cwd, dcompose.VOLUMEDIR),
		logsDir:       path.Join(cwd, dcompose.VOLUMEDIR, "logs"),
		tmpDir:        path.Join(cwd, dcompose.TMPDIR),
		availablePort: availablePort,
	}
	return runner, nil
}

// Init will initialize the state for a JobRunner. The volumeDir and logsDir
// will get created.
func (r *JobRunner) Init() error {
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
	aclCmd := exec.Command("setfacl", "-r", "-m", fmt.Sprintf("d:u:%d:rwx", uid), r.volumeDir)
	aclCmd.Stdout = logWriter
	aclCmd.Stderr = logWriter
	if err = aclCmd.Run(); err != nil {
		return err
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
	err = fs.CopyFile(fs.FS, "docker-compose.yml", path.Join(r.logsDir, "docker-compose.yml"))
	if err != nil {
		// Log error and continue.
		log.Error(err)
	}

	// Copy upload exclude list to the log dir for debugging purposes.
	err = fs.CopyFile(fs.FS, dcompose.UploadExcludesFilename, path.Join(r.logsDir, dcompose.UploadExcludesFilename))
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
	dockerBin := r.cfg.GetString("docker.path")
	// Login so that images can be pulled.
	var authinfo *authInfo
	for _, img := range r.job.ContainerImages() {
		if img.Auth != "" {
			authinfo, err = parse(img.Auth)
			if err != nil {
				return err
			}
			authCommand := exec.Command(
				dockerBin,
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
	composePath := r.cfg.GetString("docker-compose.path")
	cmd := exec.CommandContext(
		ctx,
		composePath,
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

	for index := range r.job.DataContainers() {
		svcname := fmt.Sprintf("data_%d", index)

		running(r.client, r.job, fmt.Sprintf("creating data container data_%d", index))

		if err = r.execDockerCompose(ctx, svcname, os.Environ(), logWriter, logWriter); err != nil {
			running(
				r.client,
				r.job,
				fmt.Sprintf("error creating data container data_%d: %s", index, err.Error()),
			)
			return messaging.StatusDockerCreateFailed, errors.Wrapf(err, "failed to create data container data_%d", index)
		}

		running(r.client, r.job, fmt.Sprintf("finished creating data container data_%d", index))
	}

	return messaging.Success, nil
}

func (r *JobRunner) downloadInputs(ctx context.Context) (messaging.StatusCode, error) {
	var exitCode int64

	env := os.Environ()
	env = append(env, fmt.Sprintf("VAULT_ADDR=%s", r.cfg.GetString("vault.url")))
	env = append(env, fmt.Sprintf("VAULT_TOKEN=%s", r.cfg.GetString("vault.token")))

	for index, input := range r.job.Inputs() {
		running(r.client, r.job, fmt.Sprintf("Downloading %s", input.IRODSPath()))

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
			running(r.client, r.job, fmt.Sprintf("error downloading %s: %s", input.IRODSPath(), err.Error()))
			return messaging.StatusInputFailed, errors.Wrapf(err, "failed to download %s with an exit code of %d", input.IRODSPath(), exitCode)
		}

		stdout.Close()
		stderr.Close()

		running(r.client, r.job, fmt.Sprintf("finished downloading %s", input.IRODSPath()))
	}

	return messaging.Success, nil
}

// ImageUser returns the UID of the image's default user, or 0 if it's not set.
func (r *JobRunner) ImageUser(ctx context.Context, image string) (int, error) {
	dockerPath := r.cfg.GetString("docker.path")
	out, err := exec.CommandContext(ctx, dockerPath, "image", "inspect", "-f", "{{.Config.User}}", image).Output()
	if err != nil {
		return -1, err
	}
	out = bytes.TrimSpace(out)
	if len(out) > 0 {
		return strconv.Atoi(string(out))
	}
	return 0, nil
}

// AddWorkingVolumeACL adds an ACL for the given UID to the working directory
// that gets mounted into each container that runs as part of the job. It grants
// rwx perms recursively. It is not a default ACL.
func (r *JobRunner) AddWorkingVolumeACL(ctx context.Context, uid int) error {
	cmd := exec.CommandContext(ctx, "setfacl", "-r", "-m", fmt.Sprintf("u:%d:rwx", uid), r.volumeDir)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	return cmd.Run()
}

// RemoveWorkingVolumeACL removes an ACL for the given UID from the working
// directory that gets mounted into each container that runs as part of the job.
func (r *JobRunner) RemoveWorkingVolumeACL(ctx context.Context, uid int) error {
	cmd := exec.CommandContext(ctx, "setfacl", "-r", "-x", fmt.Sprintf("u:%d", uid), r.volumeDir)
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

func (r *JobRunner) runAllSteps(parent context.Context) (messaging.StatusCode, error) {
	var err error
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	for idx, step := range r.job.Steps {
		running(r.client, r.job,
			fmt.Sprintf(
				"Running tool container %s:%s with arguments: %s",
				step.Component.Container.Image.Name,
				step.Component.Container.Image.Tag,
				strings.Join(step.Arguments(), " "),
			),
		)

		// The user ID recorded in the image is probably the user the container is
		// going to run as, so we need to make sure that user can access the files
		// in the working directory.
		imgName := fmt.Sprintf(
			"%s:%s",
			step.Component.Container.Image.Name,
			step.Component.Container.Image.Tag,
		)
		imgUID, err := r.ImageUser(ctx, imgName)
		if err != nil {
			running(r.client, r.job, fmt.Sprintf("error getting image user: %s", err.Error()))
			return messaging.StatusStepFailed, err
		}

		// This should enable the user recorded in the image to access the files in
		// the bind mounted working volume. We have a default ACL that lets the user
		// interapps-runner is running as access the files as well.
		if err = r.AddWorkingVolumeACL(ctx, imgUID); err != nil {
			running(r.client, r.job, fmt.Sprintf("error adding working volume acl: %s", err.Error()))
			return messaging.StatusStepFailed, err
		}

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
			if err = r.execDockerCompose(ctx, dcompose.ProxyServiceName(idx), os.Environ(), proxystdout, proxystderr); err != nil {
				running(r.client, r.job, fmt.Sprintf("error running proxy %s", err.Error()))
			}
		}()

		exposerURL := r.cfg.GetString("k8s.app-exposer.base")
		exposerHost := r.cfg.GetString("k8s.app-exposer.host-header")
		ingressID := dcompose.IngressID(r.job.InvocationID, r.job.UserID)

		log.Printf("creating K8s endpoint %s\n", ingressID)
		hostIP := GetOutboundIP()
		eptcfg := &EndpointConfig{
			IP:   hostIP.String(),
			Name: ingressID,
			Port: r.availablePort,
		}
		if err = CreateK8SEndpoint(exposerURL, exposerHost, eptcfg); err != nil {
			running(r.client, r.job, fmt.Sprintf("Error creating K8s Endpoint: %s", err.Error()))
			DeleteK8SEndpoint(exposerURL, exposerHost, ingressID)
			return messaging.StatusStepFailed, err
		}
		log.Printf("done creating K8s endpoint %s\n", ingressID)

		log.Printf("creating K8s service %s\n", ingressID)
		svccfg := &ServiceConfig{
			TargetPort: r.availablePort,
			Name:       ingressID,
			ListenPort: 80,
		}
		if err = CreateK8SService(exposerURL, exposerHost, svccfg); err != nil {
			running(r.client, r.job, fmt.Sprintf("Error creating K8s Service: %s", err.Error()))
			DeleteK8SService(exposerURL, exposerHost, ingressID)
			DeleteK8SEndpoint(exposerURL, exposerHost, ingressID)
			return messaging.StatusStepFailed, err
		}
		log.Printf("done creating K8s service %s\n", ingressID)

		log.Printf("creating K8s ingress %s\n", ingressID)
		ingcfg := &IngressConfig{
			Service: ingressID,
			Port:    80,
			Name:    ingressID,
		}
		if err = CreateK8SIngress(exposerURL, exposerHost, ingcfg); err != nil {
			running(r.client, r.job, fmt.Sprintf("Error creating K8s Ingress: %s", err.Error()))
			DeleteK8SIngress(exposerURL, exposerHost, ingressID)
			DeleteK8SService(exposerURL, exposerHost, ingressID)
			DeleteK8SEndpoint(exposerURL, exposerHost, ingressID)
			return messaging.StatusStepFailed, err
		}
		log.Printf("done creating K8s ingress %s\n", ingressID)

		svcname := fmt.Sprintf("step_%d", idx)
		if err = r.execDockerCompose(ctx, svcname, os.Environ(), stdout, stderr); err != nil {
			running(r.client, r.job,
				fmt.Sprintf(
					"Error running tool container %s:%s with arguments '%s': %s",
					step.Component.Container.Image.Name,
					step.Component.Container.Image.Tag,
					strings.Join(step.Arguments(), " "),
					err.Error(),
				),
			)

			return messaging.StatusStepFailed, err
		}

		running(r.client, r.job,
			fmt.Sprintf("Tool container %s:%s with arguments '%s' finished successfully",
				step.Component.Container.Image.Name,
				step.Component.Container.Image.Tag,
				strings.Join(step.Arguments(), " "),
			),
		)

		// I'm not sure if ignoring the errors here (aside from logging them) is the
		// right thing to do, but it's easy to fix if it becomes a problem. Just
		// return messaging.StatusStepFailed and the error.
		log.Printf("deleting K8s endpoint %s\n", ingressID)
		if err = DeleteK8SEndpoint(exposerURL, exposerHost, ingressID); err != nil {
			running(r.client, r.job, fmt.Sprintf("Error deleting K8s endpoint: %s", err.Error()))
		}
		log.Printf("done deleting K8s endpoint %s\n", ingressID)

		log.Printf("deleting K8s service %s\n", ingressID)
		if err = DeleteK8SService(exposerURL, exposerHost, ingressID); err != nil {
			running(r.client, r.job, fmt.Sprintf("Error deleting K8s service: %s", err.Error()))
		}
		log.Printf("done deleting K8s service %s\n", ingressID)

		log.Printf("deleting K8s ingress %s\n", ingressID)
		if err = DeleteK8SIngress(exposerURL, exposerHost, ingressID); err != nil {
			running(r.client, r.job, fmt.Sprintf("Error deleting K8s ingress: %s", err.Error()))
		}
		log.Printf("done deleting K8s ingress %s\n", ingressID)

		// Having this fail *shouldn't* be the end of the world, since everything in
		// the job working directory is going to get nuked when the job finishes
		// anyway, but we should still make an attempt to clean up as nicely as
		// possible.
		log.Println("removing working volume acl")
		if err = r.RemoveWorkingVolumeACL(ctx, imgUID); err != nil {
			running(r.client, r.job, fmt.Sprintf("error removing working volume acl: %s", err.Error()))
		}
		log.Println("done removing working volume acl")
	}

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
		fmt.Sprintf("VAULT_ADDR=%s", r.cfg.GetString("vault.url")),
		fmt.Sprintf("VAULT_TOKEN=%s", r.cfg.GetString("vault.token")),
	}

	// We're using the background context so that this stuff will run even when
	// the job is cancelled.
	if err = r.execDockerCompose(context.Background(), "upload_outputs", env, stdout, stderr); err != nil {
		running(r.client, r.job, fmt.Sprintf("Error uploading outputs to %s: %s", r.job.OutputDirectory(), err.Error()))
		return messaging.StatusOutputFailed, errors.Wrapf(err, "failed to upload outputs to %s", r.job.OutputDirectory())
	}

	running(r.client, r.job, fmt.Sprintf("Done uploading outputs to %s", r.job.OutputDirectory()))

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
func Run(ctx context.Context, client JobUpdatePublisher, job *model.Job, cfg *viper.Viper, exit chan messaging.StatusCode, availablePort int) messaging.StatusCode {
	host, err := os.Hostname()
	if err != nil {
		log.Error(err)
		host = "UNKNOWN"
	}

	runner, err := NewJobRunner(client, job, cfg, exit, availablePort)
	if err != nil {
		log.Error(err)
	}

	err = runner.Init()
	if err != nil {
		log.Error(err)
	}

	runner.projectName = strings.Replace(runner.job.InvocationID, "-", "", -1)
	runner.networkName = fmt.Sprintf("%s_default", runner.projectName)
	dockerPath := runner.cfg.GetString("docker.path")
	composePath := runner.cfg.GetString("docker-compose.path")

	// let everyone know the job is running
	running(runner.client, runner.job, fmt.Sprintf("Job %s is running on host %s", runner.job.InvocationID, host))

	if err = runner.DockerLogin(); err != nil {
		log.Error(err)
	}

	if err = runner.createNetwork(ctx, dockerPath, runner.networkName); err != nil {
		log.Error(err) // don't need to fail, since docker-compose is *supposed* to create the network
	}

	if err = runner.dockerComposePull(ctx, composePath); err != nil {
		log.Error(err)
		runner.status = messaging.StatusDockerPullFailed
	}

	if err = fs.WriteJobSummary(fs.FS, runner.logsDir, job); err != nil {
		log.Error(err)
	}

	if err = fs.WriteJobParameters(fs.FS, runner.logsDir, job); err != nil {
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
	running(runner.client, runner.job, fmt.Sprintf("Beginning to upload outputs to %s", runner.job.OutputDirectory()))
	if outputStatus, err = runner.uploadOutputs(); err != nil {
		log.Error(err)
	}
	if outputStatus != messaging.Success {
		runner.status = outputStatus
	}

	// Always inform upstream of the job status.
	if runner.status != messaging.Success {
		fail(runner.client, runner.job, fmt.Sprintf("Job exited with a status of %d", runner.status))
	} else {
		success(runner.client, runner.job)
	}

	// Clean up, you filthy animal
	downCommand := exec.Command(composePath, "-p", runner.projectName, "-f", "docker-compose.yml", "down", "-v")
	downCommand.Stderr = log.Writer()
	downCommand.Stdout = log.Writer()
	if err = downCommand.Run(); err != nil {
		log.Errorf("%+v\n", err)
	}

	netCmd := exec.Command(dockerPath, "network", "rm", fmt.Sprintf("%s_default", runner.projectName))
	netCmd.Stderr = log.Writer()
	netCmd.Stdout = log.Writer()
	if err = netCmd.Run(); err != nil {
		log.Errorf("%+v\n", err)
	}

	return runner.status
}
