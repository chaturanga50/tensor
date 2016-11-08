package runners

import (
	"fmt"
	"time"
	"bitbucket.pearson.com/apseng/tensor/models"
	log "github.com/Sirupsen/logrus"
	"os"
	"os/exec"
	"io"
	"bytes"
	"bitbucket.pearson.com/apseng/tensor/ssh"
	"strings"
	"gopkg.in/mgo.v2/bson"
	"encoding/json"
	"bitbucket.pearson.com/apseng/tensor/util"
	"strconv"
)

// JobPaths
type JobPaths struct {
	EtcTower        string
	Tmp             string
	VarLib          string
	VarLibJobStatus string
	VarLibProjects  string
	VarLog          string
	TmpRand         string
	ProjectRoot     string
	AnsiblePath     string
	CredentialPath  string
}

type AnsibleJobPool struct {
	queue    []*AnsibleJob
	Register chan *AnsibleJob
	running  []*AnsibleJob
}

var AnsiblePool = AnsibleJobPool{
	queue:    make([]*AnsibleJob, 0),
	Register: make(chan *AnsibleJob),
	running:  make([]*AnsibleJob, 0),
}

func (p *AnsibleJobPool) hasRunningJob(job *AnsibleJob) bool {
	for _, v := range p.running {
		if v.Template.ID == job.Template.ID {
			return true
		}
	}
	return false
}

func (p *AnsibleJobPool) Run() {
	ticker := time.NewTicker(2 * time.Second)

	defer func() {
		ticker.Stop()
	}()

	for {
		select {
		case job := <-p.Register:
			if job.Job.AllowSimultaneous {
				go job.run()
				continue
			}

			p.queue = append(p.queue, job)
		case <-ticker.C:
			if len(p.queue) == 0 {
				continue
			}

			job := p.queue[0]
		// if has running jobs and allow simultaneous
		// because if the job is running and AllowSimultaneous false
		// we need to make sure another instance of the same job will not run
			if p.hasRunningJob(job) && !job.Job.AllowSimultaneous {
				continue
			}

			fmt.Println("Running a task.")
			go p.queue[0].run()
			p.queue = p.queue[1:]
		}
	}
}

func (p *AnsibleJobPool) DetachFromRunning(id bson.ObjectId) bool {
	for k, v := range p.running {
		if v.Job.ID == id {
			p.running = append(p.running[:k], p.running[k + 1:]...)
			return true
		}
	}
	return false
}

// KillJob will loop through Ansible job queue and Running job queue and
// kills the job
func (p *AnsibleJobPool) KillJob(id bson.ObjectId) bool {
	// check the queue if the job is the queue then remove it
	// and update the database with cancel status
	for k, v := range p.queue {
		if v.Job.ID == id {
			// remove from job queue
			p.queue = append(p.queue[:k], p.queue[k + 1:]...)
			v.jobCancel() // update job in database
			return true
		}
	}

	for k, v := range p.running {
		if v.Job.ID == id {
			// if scm update is configured on launch
			if v.Project.ScmUpdateOnLaunch && v.Job.Status == "waiting" {
				log.Println("Sending update kill signal to job:", id.Hex())
				v.UpdateSigKill <- true
			} else {
				log.Println("Sending kill signal to job:", id.Hex())
				v.SigKill <- true //send kill signal
			}
			p.running = append(p.running[:k], p.running[k + 1:]...)
			v.jobCancel() // update job in database
			return true
		}
	}

	return false
}

func (p *AnsibleJobPool) CanCancel(id bson.ObjectId) bool {
	for _, v := range p.queue {
		if v.Job.ID == id {
			return true
		}
	}

	for _, v := range p.running {
		if v.Job.ID == id {
			return true
		}
	}

	return false
}

func CancelJob(id bson.ObjectId) bool {
	return AnsiblePool.KillJob(id)
}

func CanCancel(id bson.ObjectId) bool {
	return AnsiblePool.CanCancel(id)
}

type AnsibleJob struct {
	Job           models.Job
	Template      models.JobTemplate
	MachineCred   models.Credential
	NetworkCred   models.Credential
	CloudCred     models.Credential
	Inventory     models.Inventory
	Project       models.Project
	User          models.User
	Token         string
	JobPaths      JobPaths
	SigKill       chan bool
	UpdateSigKill chan bool
}

func (j *AnsibleJob) run() {
	//create a boolean channel to send the kill signal
	j.SigKill = make(chan bool)
	j.UpdateSigKill = make(chan bool)

	AnsiblePool.running = append(AnsiblePool.running, j)

	j.status("pending")
	log.Println("Job [" + j.Job.ID.Hex() + "] is pending:")
	// update if requested
	if j.Project.ScmUpdateOnLaunch {
		// wait for scm update
		j.status("waiting")
		log.Println("Job [" + j.Job.ID.Hex() + "] is waiting:")
		updateJob, err := UpdateProject(j.Project)

		// listen to channel
		// if true kill the channel and exit
		log.Println("Waiting for kill signal of update job:", updateJob.Job.ID.Hex())
		go func() {
			for {
				select {
				case kill := <-j.UpdateSigKill:
					log.Println("Received update kill signal:", kill)
				// kill true then kill the update job
					if kill {
						log.Println("Sending received update kill signal to updatejob:", kill)
						updateJob.SigKill <- true
					}
				}
			}
		}()

		if err != nil {
			j.Job.JobExplanation = "Previous Task Failed: {\"job_type\": \"project_update\", \"job_name\": \"" + j.Job.Name + "\", \"job_id\": \"" + updateJob.Job.ID.Hex() + "\"}"
			j.Job.ResultStdout = "stdout capture is missing"
			j.jobError()
			return
		}

		ticker := time.NewTicker(time.Second * 2)

		for range ticker.C {
			if updateJob.Job.Status == "failed" || updateJob.Job.Status == "error" {
				j.Job.JobExplanation = "Previous Task Failed: {\"job_type\": \"project_update\", \"job_name\": \"" + j.Job.Name + "\", \"job_id\": \"" + updateJob.Job.ID.Hex() + "\"}"
				j.Job.ResultStdout = "stdout capture is missing"
				j.jobError()
				return
			}
			if updateJob.Job.Status == "successful" {
				// stop the ticker and break the loop
				ticker.Stop()
				break
			}

		}
	}

	j.start()

	addActivity(j.Job.ID, j.User.ID, "Job " + j.Job.ID.Hex() + " is running")
	log.Println("Started: " + j.Job.ID.Hex() + "\n")

	//Generate directory paths and create directories
	tmp := "/tmp/tensor_proot_" + util.UniqueNew() + "/"
	j.JobPaths = JobPaths{
		EtcTower: tmp + util.UniqueNew(),
		Tmp: tmp + util.UniqueNew(),
		VarLib: tmp + util.UniqueNew(),
		VarLibJobStatus: tmp + util.UniqueNew(),
		VarLibProjects: tmp + util.UniqueNew(),
		VarLog: tmp + util.UniqueNew(),
		TmpRand: "/tmp/tensor__" + util.UniqueNew(),
		ProjectRoot: "/opt/tensor/projects/" + j.Project.ID.Hex(),
		AnsiblePath: "/opt/ansible/bin",
		CredentialPath: "/tmp/tensor_" + util.UniqueNew(),
	}

	// create job directories
	j.createJobDirs()

	// Start SSH agent
	client, socket, pid, cleanup := ssh.StartAgent()

	defer func() {
		fmt.Println("Stopped running tasks")
		AnsiblePool.DetachFromRunning(j.Job.ID)
		addActivity(j.Job.ID, j.User.ID, "Job " + j.Job.ID.Hex() + " finished")
		cleanup()
	}()

	if len(j.MachineCred.SshKeyData) > 0 {

		if len(j.MachineCred.SshKeyUnlock) > 0 {
			key, err := ssh.GetEncryptedKey([]byte(util.CipherDecrypt(j.MachineCred.SshKeyData)), util.CipherDecrypt(j.MachineCred.SshKeyUnlock))
			if err != nil {
				log.Println("Error while decyrpting Machine Credential", err)
				j.Job.JobExplanation = err.Error()
				j.jobFail()
				return
			}
			if client.Add(key); err != nil {
				log.Println("Error while adding decyrpted Machine Credential to SSH Agent", err)
				j.Job.JobExplanation = err.Error()
				j.jobFail()
				return
			}
		}

		key, err := ssh.GetKey([]byte(util.CipherDecrypt(j.MachineCred.SshKeyData)))
		if err != nil {
			log.Println("Error while decyrpting Machine Credential", err)
			j.Job.JobExplanation = err.Error()
			j.jobFail()
			return
		}

		if client.Add(key); err != nil {
			log.Println("Error while adding decyrpted Machine Credential to SSH Agent", err)
			j.Job.JobExplanation = err.Error()
			j.jobFail()
			return
		}

	}

	if len(j.NetworkCred.SshKeyData) > 0 {
		if len(j.NetworkCred.SshKeyUnlock) > 0 {
			key, err := ssh.GetEncryptedKey([]byte(util.CipherDecrypt(j.MachineCred.SshKeyData)), util.CipherDecrypt(j.NetworkCred.SshKeyUnlock))
			if err != nil {
				log.Println("Error while decyrpting Machine Credential", err)
				j.Job.JobExplanation = err.Error()
				j.jobFail()
				return
			}
			if client.Add(key); err != nil {
				log.Println("Error while adding decyrpted Machine Credential to SSH Agent", err)
				j.Job.JobExplanation = err.Error()
				j.jobFail()
				return
			}
		}

		key, err := ssh.GetKey([]byte(util.CipherDecrypt(j.MachineCred.SshKeyData)))
		if err != nil {
			log.Println("Error while decyrpting Machine Credential", err)
			j.Job.JobExplanation = err.Error()
			j.jobFail()
			return
		}

		if client.Add(key); err != nil {
			log.Println("Error while adding decyrpted Machine Credential to SSH Agent", err)
			j.Job.JobExplanation = err.Error()
			j.jobFail()
			return
		}

	}

	cmd, err := j.getCmd(socket, pid);
	if err != nil {
		log.Println("Running playbook failed", err)
		j.Job.ResultStdout = "stdout capture is missing"
		j.Job.JobExplanation = err.Error()
		j.jobFail()
		return
	}

	// To make sure the SigKill not execute before job starts
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b

	if err := cmd.Start(); err != nil {
		if err != nil {
			log.Println("Running playbook failed", err)
			j.Job.JobExplanation = err.Error()
			j.jobFail()
			return
		}
	}

	// listen to channel
	// if true kill the channel and exit
	go func() {
		for {
			select {
			case kill := <-j.SigKill:
			// kill true then kill the job
				if kill {
					if err := cmd.Process.Kill(); err != nil {
						log.Println("Could not cancel the job")
						return // exit from goroutine
					}
					j.jobCancel() // update cancelled status
				}
			}
		}
	}()

	waitErr := cmd.Wait();
	// set stdout if
	j.Job.ResultStdout = string(b.Bytes())
	if waitErr != nil {
		log.Println("Running playbook failed", waitErr)
		j.Job.JobExplanation = waitErr.Error()
		j.jobFail()
		return
	}

	//success
	j.jobSuccess()
}

// runPlaybook runs a Job using ansible-playbook command
func (j *AnsibleJob) getCmd(socket string, pid int) (*exec.Cmd, error) {

	// ansible-playbook parameters
	pPlaybook := []string{
		"-i", "/opt/tensor/plugins/inventory/tensorrest.py",
	}
	pPlaybook = j.buildParams(pPlaybook)

	// parameters that are hidden from output
	pSecure := []string{}

	// check whether the username not empty
	if len(j.MachineCred.Username) > 0 {
		uname := j.MachineCred.Username

		// append domain if exist
		if len(j.MachineCred.Domain) > 0 {
			uname = j.MachineCred.Username + "@" + j.MachineCred.Domain
		}

		pPlaybook = append(pPlaybook, "-u", uname)

		if len(j.MachineCred.Password) > 0 && j.MachineCred.Kind == models.CREDENTIAL_KIND_SSH {
			pSecure = append(pSecure, "-e", "ansible_ssh_pass=" + util.CipherDecrypt(j.MachineCred.Password) + "")
		}

		// if credential type is windows the issue a kinit to acquire a kerberos ticket
		if len(j.MachineCred.Password) > 0 && j.MachineCred.Kind == models.CREDENTIAL_KIND_WIN {
			j.kinit()
		}
	}

	if j.Job.BecomeEnabled {
		pPlaybook = append(pPlaybook, "-b")

		// default become method is sudo
		if len(j.MachineCred.BecomeMethod) > 0 {
			pPlaybook = append(pPlaybook, "--become-method=" + j.MachineCred.BecomeMethod)
		}

		// default become user is root
		if len(j.MachineCred.BecomeUsername) > 0 {
			pPlaybook = append(pPlaybook, "--become-user=" + j.MachineCred.BecomeUsername)
		}

		// for now this is more convenient than --ask-become-pass with sshpass
		if len(j.MachineCred.BecomePassword) > 0 {
			pSecure = append(pSecure, "-e", "'ansible_become_pass=" + util.CipherDecrypt(j.MachineCred.BecomePassword) + "'")
		}
	}

	pargs := []string{}
	// add proot and ansible paramters
	pargs = append(pargs, pPlaybook...)
	j.Job.JobARGS = pargs
	// should not included in any output
	pargs = append(pargs, pSecure...)

	// set job arguments, exclude unencrypted passwords etc.
	j.Job.JobARGS = []string{strings.Join(j.Job.JobARGS, " ") + " " + j.Job.Playbook + "'"}

	// For example, if I type something like:
	// $ exec /usr/bin/ssh-agent /bin/bash
	// from my shell prompt, I end up in a bash that is setup correctly with the agent.
	// As soon as that bash dies, or any process that replaced bash with exec dies, the agent exits.
	// add -c for shell, yes it's ugly but meh! this is golden
	pargs = append(pargs, j.Job.Playbook)

	cmd := exec.Command("ansible-playbook", pargs...)
	cmd.Dir = "/opt/tensor/projects/" + j.Project.ID.Hex()

	cmd.Env = []string{
		"TERM=xterm",
		"PROJECT_PATH=/opt/tensor/projects/" + j.Project.ID.Hex(),
		"HOME_PATH=/opt/tensor/",
		"PWD=/opt/tensor/projects/" + j.Project.ID.Hex(),
		"SHLVL=1",
		"HOME=/root",
		"_=/opt/tensor/bin/tensord",
		"PATH=/bin:/usr/local/go/bin:/opt/tensor/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"REST_API_TOKEN=" + j.Token,
		"ANSIBLE_PARAMIKO_RECORD_HOST_KEYS=False",
		"ANSIBLE_CALLBACK_PLUGINS=/opt/tensor/plugins/callback",
		"ANSIBLE_HOST_KEY_CHECKING=False",
		"JOB_ID=" + j.Job.ID.Hex(),
		"ANSIBLE_FORCE_COLOR=True",
		"REST_API_URL=http://localhost:8010",
		"INVENTORY_HOSTVARS=True",
		"INVENTORY_ID=" + j.Inventory.ID.Hex(),
		"SSH_AUTH_SOCK=" + socket,
		"SSH_AGENT_PID=" + strconv.Itoa(pid),
	}

	j.Job.JobENV = cmd.Env

	return cmd, nil
}

// createJobDirs
func (j *AnsibleJob) createJobDirs() {
	// create credential paths
	if err := os.MkdirAll(j.JobPaths.EtcTower, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.EtcTower)
	}
	if err := os.MkdirAll(j.JobPaths.CredentialPath, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.CredentialPath)
	}
	if err := os.MkdirAll(j.JobPaths.Tmp, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.Tmp)
	}
	if err := os.MkdirAll(j.JobPaths.TmpRand, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.TmpRand)
	}
	if err := os.MkdirAll(j.JobPaths.VarLib, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.VarLib)
	}
	if err := os.MkdirAll(j.JobPaths.VarLibJobStatus, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.VarLibJobStatus)
	}
	if err := os.MkdirAll(j.JobPaths.VarLibProjects, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.VarLibProjects)
	}
	if err := os.MkdirAll(j.JobPaths.VarLog, 0770); err != nil {
		log.Println("Unable to create directory: ", j.JobPaths.VarLog)
	}
}

func (j *AnsibleJob) buildParams(params []string) []string {

	if j.Job.JobType == "check" {
		params = append(params, "--check")
	}

	// forks -f NUM, --forks=NUM
	if j.Job.Forks != 0 {
		params = append(params, "-f", string(j.Job.Forks))
	}

	// limit  -l SUBSET, --limit=SUBSET
	if j.Job.Limit != "" {
		params = append(params, "-l", j.Job.Limit)
	}

	// verbosity  -v, --verbose
	switch j.Job.Verbosity {
	case 1:
		params = append(params, "-v")
		break
	case 2:
		params = append(params, "-vv")
		break
	case 3:
		params = append(params, "-vvv")
		break
	case 4:
		params = append(params, "-vvvv")
		break
	case 5:
		params = append(params, "-vvvv")
	}

	// extra variables -e EXTRA_VARS, --extra-vars=EXTRA_VARS
	if len(j.Job.ExtraVars) > 0 {
		vars, err := json.Marshal(j.Job.ExtraVars)
		if err != nil {
			log.Println("Could not marshal extra vars", err)
		}
		params = append(params, "-e", "'" + string(vars) + "'")
	}

	// -t, TAGS, --tags=TAGS
	if len(j.Job.JobTags) > 0 {
		params = append(params, "-t", j.Job.JobTags)
	}

	// --skip-tags=SKIP_TAGS
	if len(j.Job.SkipTags) > 0 {
		params = append(params, "--skip-tags=" + j.Job.SkipTags)
	}

	// --force-handlers
	if j.Job.ForceHandlers {
		params = append(params, "--force-handlers")
	}

	if len(j.Job.StartAtTask) > 0 {
		params = append(params, "--start-at-task=" + j.Job.StartAtTask)
	}

	extras := map[string]interface{}{
		"tensor_job_template_name": j.Template.Name,
		"tensor_job_id": j.Job.ID.Hex(),
		"tensor_user_id": j.Job.CreatedByID.Hex(),
		"tensor_job_template_id": j.Template.ID.Hex(),
		"tensor_user_name": "admin",
		"tensor_job_launch_type": j.Job.LaunchType,
	}
	// Parameters required by the system
	rp, err := json.Marshal(extras);

	if err != nil {
		log.Println("Error while marshalling parameters")
	}
	params = append(params, "-e", string(rp))

	return params
}

func (j *AnsibleJob) kinit() error {

	// Create two command structs for echo and kinit
	echo := exec.Command("echo", "-n", util.CipherDecrypt(j.MachineCred.Password))

	uname := j.MachineCred.Username

	// if credential domain specified
	if len(j.MachineCred.Domain) > 0 {
		uname = j.MachineCred.Username + "@" + j.MachineCred.Domain
	}

	kinit := exec.Command("kinit", uname)
	kinit.Env = os.Environ()

	// Create asynchronous in memory pipe
	r, w := io.Pipe()

	// set pipe writer to echo std out
	echo.Stdout = w
	// set pip reader to kinit std in
	kinit.Stdin = r

	// initialize new buffer
	var buffer bytes.Buffer
	kinit.Stdout = &buffer

	// start two commands
	if err := echo.Start(); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := kinit.Start(); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := echo.Wait(); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := w.Close(); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := kinit.Wait(); err != nil {
		log.Println(err.Error())
		return err
	}

	if _, err := io.Copy(os.Stdout, &buffer); err != nil {
		log.Println(err.Error())
		return err
	}

	return nil
}