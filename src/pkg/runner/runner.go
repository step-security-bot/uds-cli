// SPDX-License-Identifier: Apache-2.0

// Package runner provides functions for running tasks in a run.yaml
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	// used for compile time directives to pull functions from Zarf
	_ "unsafe"

	"github.com/defenseunicorns/zarf/src/config/lang"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/utils"
	"github.com/defenseunicorns/zarf/src/pkg/utils/helpers"
	zarfTypes "github.com/defenseunicorns/zarf/src/types"
	"github.com/mholt/archiver/v3"

	"github.com/defenseunicorns/uds-cli/src/types"
)

// Runner holds the necessary data to run tasks from a tasks file
type Runner struct {
	TemplateMap map[string]*utils.TextTemplate
	TasksFile   types.TasksFile
	TaskNameMap map[string]bool
}

// Run runs a task from tasks file
func Run(tasksFile types.TasksFile, taskName string) error {
	runner := Runner{
		TemplateMap: map[string]*utils.TextTemplate{},
		TasksFile:   tasksFile,
		TaskNameMap: map[string]bool{},
	}

	task, err := runner.getTask(taskName)
	if err != nil {
		return err
	}

	runner.populateTemplateMap(tasksFile.Variables)

	err = runner.executeTask(task)
	return err
}

func (r *Runner) getTask(taskName string) (types.Task, error) {
	for _, task := range r.TasksFile.Tasks {
		if task.Name == taskName {
			return task, nil
		}
	}
	return types.Task{}, fmt.Errorf("task name %s not found", taskName)
}

func (r *Runner) executeTask(task types.Task) error {
	if len(task.Files) > 0 {
		if err := r.placeFiles(task.Files); err != nil {
			return err
		}
	}

	for _, action := range task.Actions {
		if err := r.performAction(action); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) populateTemplateMap(zarfVariables []zarfTypes.ZarfPackageVariable) {
	for _, variable := range zarfVariables {
		r.TemplateMap[fmt.Sprintf("${%s}", variable.Name)] = &utils.TextTemplate{
			Sensitive:  variable.Sensitive,
			AutoIndent: variable.AutoIndent,
			Type:       variable.Type,
			Value:      variable.Default,
		}
	}
}

func (r *Runner) placeFiles(files []zarfTypes.ZarfFile) error {
	for _, file := range files {
		// template file.Source and file.Target
		srcFile := r.templateString(file.Source)
		targetFile := r.templateString(file.Target)

		// get current directory
		workingDir, err := os.Getwd()
		if err != nil {
			return err
		}
		dest := filepath.Join(workingDir, targetFile)
		destDir := filepath.Dir(dest)

		if helpers.IsURL(srcFile) {

			// If file is a url download it
			if err := utils.DownloadToFile(srcFile, dest, ""); err != nil {
				return fmt.Errorf(lang.ErrDownloading, srcFile, err.Error())
			}
		} else {
			// If file is not a url copy it
			if err := utils.CreatePathAndCopy(srcFile, dest); err != nil {
				return fmt.Errorf("unable to copy file %s: %w", srcFile, err)
			}

		}
		// If file has extract path extract it
		if file.ExtractPath != "" {
			_ = os.RemoveAll(file.ExtractPath)
			err = archiver.Extract(dest, file.ExtractPath, destDir)
			if err != nil {
				return fmt.Errorf(lang.ErrFileExtract, file.ExtractPath, srcFile, err.Error())
			}
		}

		// if shasum is specified check it
		if file.Shasum != "" {
			if file.ExtractPath != "" {
				if err := utils.SHAsMatch(file.ExtractPath, file.Shasum); err != nil {
					return err
				}
			} else {
				if err := utils.SHAsMatch(dest, file.Shasum); err != nil {
					return err
				}
			}
		}

		// template any text files with variables
		fileList := []string{}
		if utils.IsDir(dest) {
			files, _ := utils.RecursiveFileList(dest, nil, false)
			fileList = append(fileList, files...)
		} else {
			fileList = append(fileList, dest)
		}
		for _, subFile := range fileList {
			// Check if the file looks like a text file
			isText, err := utils.IsTextFile(subFile)
			if err != nil {
				fmt.Printf("unable to determine if file %s is a text file: %s", subFile, err)
			}

			// If the file is a text file, template it
			if isText {
				if err := utils.ReplaceTextTemplate(subFile, r.TemplateMap, nil, `\$\{[A-Z0-9_]+\}`); err != nil {
					return fmt.Errorf("unable to template file %s: %w", subFile, err)
				}
			}
		}

		// if executable make file executable
		if file.Executable || utils.IsDir(dest) {
			_ = os.Chmod(dest, 0700)
		} else {
			_ = os.Chmod(dest, 0600)
		}

		// if symlinks create them
		for _, link := range file.Symlinks {
			// Try to remove the filepath if it exists
			_ = os.RemoveAll(link)
			// Make sure the parent directory exists
			_ = utils.CreateFilePath(link)
			// Create the symlink
			err := os.Symlink(targetFile, link)
			if err != nil {
				return fmt.Errorf("unable to create symlink %s->%s: %w", link, targetFile, err)
			}
		}
	}
	return nil
}

func (r *Runner) performAction(action types.Action) error {
	if action.TaskReference != "" {
		referencedTask, err := r.getTask(action.TaskReference)
		if err != nil {
			return err
		}
		if err := r.checkForTaskLoops(referencedTask); err != nil {
			return err
		}
		if err := r.executeTask(referencedTask); err != nil {
			return err
		}
	} else {
		err := r.performZarfAction(action.ZarfComponentAction)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) checkForTaskLoops(task types.Task) error {
	for _, action := range task.Actions {
		if action.TaskReference != "" {
			exists := r.TaskNameMap[action.TaskReference]
			if exists {
				return fmt.Errorf("task loop detected")
			}
			r.TaskNameMap[action.TaskReference] = true
			newTask, err := r.getTask(action.TaskReference)
			if err != nil {
				return err
			}
			if err = r.checkForTaskLoops(newTask); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) performZarfAction(action *zarfTypes.ZarfComponentAction) error {
	var (
		ctx        context.Context
		cancel     context.CancelFunc
		cmdEscaped string
		out        string
		err        error

		cmd = action.Cmd
	)

	// If the action is a wait, convert it to a command.
	if action.Wait != nil {
		// If the wait has no timeout, set a default of 5 minutes.
		if action.MaxTotalSeconds == nil {
			fiveMin := 300
			action.MaxTotalSeconds = &fiveMin
		}

		// Convert the wait to a command.
		if cmd, err = convertWaitToCmd(*action.Wait, action.MaxTotalSeconds); err != nil {
			return err
		}

		// Mute the output becuase it will be noisy.
		t := true
		action.Mute = &t

		// Set the max retries to 0.
		z := 0
		action.MaxRetries = &z

		// Not used for wait actions.
		d := ""
		action.Dir = &d
		action.Env = []string{}
		action.SetVariables = []zarfTypes.ZarfComponentActionSetVariable{}
	}

	if action.Description != "" {
		cmdEscaped = action.Description
	} else {
		cmdEscaped = message.Truncate(cmd, 60, false)
	}

	spinner := message.NewProgressSpinner("Running \"%s\"", cmdEscaped)
	// Persist the spinner output so it doesn't get overwritten by the command output.
	spinner.EnablePreserveWrites()

	// If the value template is not nil, get the variables for the action.
	// No special variables or deprecations will be used in the action.
	// Reload the variables each time in case they have been changed by a previous action.
	// if valueTemplate != nil {
	// 	vars, _ = valueTemplate.GetVariables(zarfTypes.ZarfComponent{})
	// }

	cfg := actionGetCfg(zarfTypes.ZarfComponentActionDefaults{}, *action, r.TemplateMap)

	if cmd, err = actionCmdMutation(cmd); err != nil {
		spinner.Errorf(err, "Error mutating command: %s", cmdEscaped)
	}

	// template cmd string
	cmd = r.templateString(cmd)

	duration := time.Duration(cfg.MaxTotalSeconds) * time.Second
	timeout := time.After(duration)

	// Keep trying until the max retries is reached.
	for remaining := cfg.MaxRetries + 1; remaining > 0; remaining-- {

		// Perform the action run.
		tryCmd := func(ctx context.Context) error {
			// Try running the command and continue the retry loop if it fails.
			if out, err = actionRun(ctx, cfg, cmd, cfg.Shell, spinner); err != nil {
				return err
			}

			out = strings.TrimSpace(out)

			// If an output variable is defined, set it.
			for _, v := range action.SetVariables {
				// include ${...} syntax in template map for uniformity and to satisfy utils.ReplaceTextTemplate
				nameInTemplatemap := "${" + v.Name + "}"
				r.TemplateMap[nameInTemplatemap] = &utils.TextTemplate{
					Sensitive:  v.Sensitive,
					AutoIndent: v.AutoIndent,
					Type:       v.Type,
					Value:      out,
				}
				if regexp.MustCompile(v.Pattern).MatchString(r.TemplateMap[nameInTemplatemap].Value); err != nil {
					message.WarnErr(err, err.Error())
					return err
				}
			}

			// If the action has a wait, change the spinner message to reflect that on success.
			if action.Wait != nil {
				spinner.Successf("Wait for \"%s\" succeeded", cmdEscaped)
			} else {
				spinner.Successf("Completed \"%s\"", cmdEscaped)
			}

			// If the command ran successfully, continue to the next action.
			return nil
		}

		// If no timeout is set, run the command and return or continue retrying.
		if cfg.MaxTotalSeconds < 1 {
			spinner.Updatef("Waiting for \"%s\" (no timeout)", cmdEscaped)
			if err := tryCmd(context.TODO()); err != nil {
				continue
			}

			return nil
		}

		// Run the command on repeat until success or timeout.
		spinner.Updatef("Waiting for \"%s\" (timeout: %ds)", cmdEscaped, cfg.MaxTotalSeconds)
		select {
		// On timeout break the loop to abort.
		case <-timeout:
			break

		// Otherwise, try running the command.
		default:
			ctx, cancel = context.WithTimeout(context.Background(), duration)
			defer cancel()
			if err := tryCmd(ctx); err != nil {
				continue
			}

			return nil
		}
	}

	select {
	case <-timeout:
		// If we reached this point, the timeout was reached.
		return fmt.Errorf("command \"%s\" timed out after %d seconds", cmdEscaped, cfg.MaxTotalSeconds)

	default:
		// If we reached this point, the retry limit was reached.
		return fmt.Errorf("command \"%s\" failed after %d retries", cmdEscaped, cfg.MaxRetries)
	}
}

func (r *Runner) templateString(s string) string {
	// Create a regular expression to match ${...}
	re := regexp.MustCompile(`\${(.*?)}`)

	// template string using values from the template map
	result := re.ReplaceAllStringFunc(s, func(matched string) string {
		if value, ok := r.TemplateMap[matched]; ok {
			return value.Value
		}
		return matched // If the key is not found, keep the original substring
	})
	return result
}

// Perform some basic string mutations to make commands more useful.
func actionCmdMutation(cmd string) (string, error) {
	runCmd, err := utils.GetFinalExecutablePath()
	if err != nil {
		return cmd, err
	}

	// Try to patch the binary path in case the name isn't exactly "./uds".
	cmd = strings.ReplaceAll(cmd, "./uds ", runCmd+" ")

	return cmd, nil
}

// convertWaitToCmd will return the wait command if it exists, otherwise it will return the original command.
func convertWaitToCmd(wait zarfTypes.ZarfComponentActionWait, timeout *int) (string, error) {
	// Build the timeout string.
	timeoutString := fmt.Sprintf("--timeout %ds", *timeout)

	// If the action has a wait, build a cmd from that instead.
	cluster := wait.Cluster
	if cluster != nil {
		ns := cluster.Namespace
		if ns != "" {
			ns = fmt.Sprintf("-n %s", ns)
		}

		// Build a call to the uds tools wait-for command.
		return fmt.Sprintf("./uds tools wait-for %s %s %s %s %s",
			cluster.Kind, cluster.Identifier, cluster.Condition, ns, timeoutString), nil
	}

	network := wait.Network
	if network != nil {
		// Make sure the protocol is lower case.
		network.Protocol = strings.ToLower(network.Protocol)

		// If the protocol is http and no code is set, default to 200.
		if strings.HasPrefix(network.Protocol, "http") && network.Code == 0 {
			network.Code = 200
		}

		// Build a call to the uds tools wait-for command.
		return fmt.Sprintf("./uds tools wait-for %s %s %d %s",
			network.Protocol, network.Address, network.Code, timeoutString), nil
	}

	return "", fmt.Errorf("wait action is missing a cluster or network")
}

//go:linkname actionGetCfg github.com/defenseunicorns/zarf/src/pkg/packager.actionGetCfg
func actionGetCfg(cfg zarfTypes.ZarfComponentActionDefaults, a zarfTypes.ZarfComponentAction, vars map[string]*utils.TextTemplate) zarfTypes.ZarfComponentActionDefaults

//go:linkname actionRun github.com/defenseunicorns/zarf/src/pkg/packager.actionRun
func actionRun(ctx context.Context, cfg zarfTypes.ZarfComponentActionDefaults, cmd string, shellPref zarfTypes.ZarfComponentActionShell, spinner *message.Spinner) (string, error)