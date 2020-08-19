package testworker

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beeker1121/goque"

	"github.com/douyu/juno/internal/pkg/service/codeplatform"
	"github.com/douyu/juno/internal/pkg/service/testplatform/pipeline"
	"github.com/douyu/juno/pkg/model/db"
	"github.com/douyu/juno/pkg/model/view"
	"github.com/douyu/jupiter/pkg/xlog"
	"github.com/go-resty/resty/v2"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

type (
	TestWorker struct {
		option   Option
		client   *resty.Client
		taskChan chan view.TestTask
		queue    *goque.Queue
	}

	Option struct {
		JunoAddress    string
		Token          string
		ParallelWorker int
		RepoStorageDir string
		QueueDir       string
	}

	RespConsumeJob struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data *view.TestTask `json:"data"`
	}

	ProgressLog struct {
		ProgressLog bool         `json:"progress_log"` // always true
		Type        progressType `json:"type"`         // "error" | "start"
		Msg         string       `json:"msg"`
	}

	progressType string
)

var (
	instance *TestWorker
	initOnce sync.Once

	progressStart   progressType = "start"
	progressSuccess progressType = "success"
	progressFailed  progressType = "failed"
)

func Instance() *TestWorker {
	initOnce.Do(func() {
		instance = &TestWorker{
			taskChan: make(chan view.TestTask),
		}
	})

	return instance
}

func (t *TestWorker) Init(option Option) (err error) {
	t.option = option
	t.client = resty.New().
		SetHostURL(option.JunoAddress).
		SetTimeout(20*time.Second).
		SetHeader("Token", option.Token)
	t.queue, err = goque.OpenQueue(option.QueueDir)
	if err != nil {
		return
	}

	t.Start()

	return
}

func (t *TestWorker) Start() {
	go t.startPull()
	go t.startWork()
}

func (t *TestWorker) Push(task view.TestTask) error {
	_, err := t.queue.EnqueueObjectAsJSON(task)
	if err != nil {
		xlog.Error("enqueue failed", xlog.String("err", err.Error()))
		return err
	}

	return nil
}

func (t *TestWorker) startPull() {
	for {
		item, err := t.queue.Dequeue()
		if err != nil {
			if err == goque.ErrEmpty {
				time.Sleep(1 * time.Second)
			} else {
				xlog.Error("pull item failed. wait for 10 second and retry", xlog.String("err", err.Error()))
				time.Sleep(10 * time.Second)
			}

			continue
		}

		if item == nil {
			continue
		}

		var task view.TestTask
		err = item.ToObjectFromJSON(&task)
		if err != nil {
			xlog.Error("unmarshall task failed", xlog.String("err", err.Error()))

			continue
		}

		t.taskChan <- task
	}
}

func (t *TestWorker) startWork() {
	for i := 0; i < t.option.ParallelWorker; i++ {
		go t.work()
	}
}

func (t *TestWorker) work() {
	for {
		task := <-t.taskChan

		t.notifyTaskUpdate(task.TaskID, db.TestTaskStatusRunning, "")

		err := t.runTask(task, task.Desc)
		if err != nil {
			t.notifyTaskUpdate(task.TaskID, db.TestTaskStatusFailed, fmt.Sprintf("task failed. err = %s", err.Error()))
		} else {
			t.notifyTaskUpdate(task.TaskID, db.TestTaskStatusSuccess, "")
		}
	}
}

func (t *TestWorker) runTask(task view.TestTask, desc db.TestPipelineDesc) (err error) {
	eg := errgroup.Group{}
	for _, step := range desc.Steps {
		if desc.Parallel {
			eg.Go(func() error {
				return t.runStep(task, step)
			})
		} else {
			err = t.runStep(task, step)
			if err != nil {
				xlog.Error("TestWorker.runTask failed, stop running", xlog.String("err", err.Error()))
				break
			}
		}
	}
	if err != nil {
		return
	}

	err = eg.Wait()
	if err != nil {
		return
	}

	return
}

func (t *TestWorker) runStep(task view.TestTask, step db.TestPipelineStep) (err error) {
	switch step.Type {
	case db.StepTypeJob:
		if step.JobPayload == nil {
			return fmt.Errorf("platform.JobPayload = nil when step.Type = StepTypeJob. step = %v", step)
		}

		err = t.runJob(task, step.Name, step.JobPayload)
		if err != nil {
			return
		}

	case db.StepTypeSubPipeline:
		if step.SubPipeline != nil {
			err = t.runTask(task, *step.SubPipeline)
			if err != nil {
				return
			}
		} else {
			return fmt.Errorf("platform.SubPipeline = nil when step.Type = StepTypeSubPipeline. step = %v", step)
		}
	}

	return
}

func (t *TestWorker) runJob(task view.TestTask, name string, payload *db.TestJobPayload) (err error) {
	t.notifyProgress(task.TaskID, name, db.TestTaskStatusRunning, progressStart, "")

	switch payload.Type {
	case db.JobGitPull:
		err = t.gitPull(task, name, payload.Payload)

	case db.JobUnitTest:
		err = t.unitTest(task, name, payload.Payload)

	case db.JobCodeCheck:
		err = t.codeCheck(task, name, payload.Payload)
	}
	if err != nil {
		xlog.Error("runJob failed", xlog.String("err", err.Error()))
	}

	return
}

func (t *TestWorker) notifyTaskEvent(taskId uint, event view.TestTaskEventType, data interface{}) {
	req := t.client.R()

	eventData, _ := json.Marshal(data)
	body := view.TestTaskEvent{
		Type:   event,
		TaskID: taskId,
		Data:   eventData,
	}

	req.SetBody(body)

	resp, err := req.Post("/api/v1/worker/testTask/update")
	if err != nil {
		log.Error("TestWorker.notifyStepStatus", xlog.String("err", err.Error()))
		return
	}

	respObj := struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}{}
	err = json.Unmarshal(resp.Body(), &respObj)
	if err != nil {
		log.Error("TestWorker: json unmarshall failed", xlog.String("err", err.Error()))
		return
	}

}

func (t *TestWorker) notifyTaskUpdate(taskId uint, status db.TestTaskStatus, logsAppend string) {
	t.notifyTaskEvent(taskId, view.TaskUpdateEvent, view.TestTaskUpdateEventPayload{
		Status:     status,
		LogsAppend: logsAppend,
	})
}

func (t *TestWorker) notifyStepStatus(taskId uint, stepName string, status db.TestStepStatus, logsAppend string) {
	data := view.TestTaskStepUpdatePayload{
		StepName:   stepName,
		Status:     status,
		LogsAppend: logsAppend,
	}

	t.notifyTaskEvent(taskId, view.TaskStepUpdateEvent, data)
}

func (t *TestWorker) codeBaseDir(task view.TestTask) string {
	return filepath.Join(t.option.RepoStorageDir, task.AppName, task.Branch)
}

func (t *TestWorker) gitPull(task view.TestTask, name string, p json.RawMessage) (err error) {
	var progress string
	var payload pipeline.JobGitPullPayload

	defer func() {
		if err != nil {
			// failed
			t.notifyStepStatus(task.TaskID, name, db.TestStepStatusFailed, fmt.Sprintf("%s\nerr = %s", progress, err.Error()))
		} else {
			// success
			t.notifyStepStatus(task.TaskID, name, db.TestStepStatusSuccess, progress)
		}
	}()

	err = json.Unmarshal(p, &payload)
	if err != nil {
		return errors.Wrapf(err, "unmarshall payload into pipeline.JobGitPullPayload failed. err = %s", err.Error())
	}

	code := codeplatform.New(codeplatform.Option{
		StorageDir: t.codeBaseDir(task),
		Token:      payload.AccessToken,
	})

	progress, err = code.CloneOrPull(payload.GitHttpUrl, t.codeBaseDir(task))
	if err != nil {
		return err
	}

	return nil
}

func (t *TestWorker) unitTest(task view.TestTask, name string, p json.RawMessage) (err error) {
	var payload pipeline.JobUnitTestPayload
	printer := NewPrinter(128)

	defer func() {
		logs := printer.Flush()

		if err != nil {
			t.notifyStepStatus(task.TaskID, name, db.TestStepStatusFailed, string(logs))
			t.notifyProgress(task.TaskID, name, db.TestTaskStatusFailed, progressFailed, err.Error())
		} else {
			t.notifyStepStatus(task.TaskID, name, db.TestTaskStatusSuccess, string(logs))
			t.notifyProgress(task.TaskID, name, db.TestTaskStatusFailed, progressSuccess, "")
		}
	}()

	err = json.Unmarshal(p, &payload)
	if err != nil {
		return errors.Wrapf(err, "unmarshall payload into pipeline.JobUnitTestPayload failed. err = %s", err.Error())
	}

	gitUrlParsed, err := url.Parse(task.GitUrl)
	if err != nil {
		return errors.Wrapf(err, "invalid GitUrl")
	}

	cmdArray := []string{
		fmt.Sprintf("git config --global url.\"https://juno:%s@%s/\".insteadOf \"https://%s/\"", payload.AccessToken, gitUrlParsed.Host, gitUrlParsed.Host),
		fmt.Sprintf("cd %s", t.codeBaseDir(task)),
		"go test -v -json ./...",
	}
	cmd := exec.Command("sh", "-c", strings.Join(cmdArray, " && "))
	cmd.Stdout = printer
	cmd.Stderr = printer
	finishChan := make(chan error, 1)
	timer := time.NewTimer(5 * time.Minute)

	go func() {
		finishChan <- cmd.Run()
		_ = exec.Command(fmt.Sprintf("git config --global --remove-section url.\"https://juno:%s@%s/\"", payload.AccessToken, gitUrlParsed.Host)).Run()
	}()

	for {
		select {
		case logs := <-printer.C:
			fmt.Printf("\n-> printer logs: %s\n", logs)
			t.notifyStepStatus(task.TaskID, name, db.TestStepStatusRunning, logs)

		case <-timer.C: // timeout
			close(finishChan)
			err = cmd.Process.Kill()
			if err != nil {
				err = errors.Wrap(err, "unitTest process kill failed")
				return
			}

			return fmt.Errorf("unitTest process timeout. killed")

		case err = <-finishChan:
			return
		}
	}
}

func (t *TestWorker) notifyProgress(taskId uint, stepName string, status db.TestStepStatus, progressType progressType, msg string) {
	logs, _ := json.Marshal(ProgressLog{
		ProgressLog: true,
		Type:        progressType,
		Msg:         msg,
	})
	t.notifyStepStatus(taskId, stepName, status, string(logs)+"\n")
}

func (t *TestWorker) codeCheck(task view.TestTask, name string, p json.RawMessage) error {
	dir := filepath.Join(t.codeBaseDir(task), "/...")
	dir = strings.Replace(dir, string(filepath.Separator), "/", -1)
	linter := NewLinter(dir)
	problems, err := linter.Lint()
	logs := ""
	for _, problem := range problems {
		problemBytes, _ := json.Marshal(problem)
		logs += string(problemBytes) + "\n"
	}
	t.notifyStepStatus(task.TaskID, name, db.TestStepStatusRunning, logs)

	if err != nil {
		t.notifyProgress(task.TaskID, name, db.TestStepStatusFailed, progressFailed, err.Error())
	} else {
		t.notifyProgress(task.TaskID, name, db.TestStepStatusSuccess, progressSuccess, "")
	}

	return nil
}