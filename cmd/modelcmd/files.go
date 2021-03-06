// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package modelcmd

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/utils/fslock"

	"github.com/juju/juju/juju/osenv"
)

const (
	CurrentModelFilename      = "current-model"
	CurrentControllerFilename = "current-controller"

	lockName = "current.lock"

	controllerSuffix = " (controller)"
)

var (
	// 5 seconds should be way more than enough to write or read any files
	// even on heavily loaded controllers.
	lockTimeout = 5 * time.Second
)

// ServerFile describes the information that is needed for a user
// to connect to an api server.
type ServerFile struct {
	Addresses []string `yaml:"addresses"`
	CACert    string   `yaml:"ca-cert,omitempty"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`
}

// NOTE: synchronisation across functions in this file.
//
// Each of the read and write functions use a fslock to synchronise calls
// across both the current executable and across different executables.

func getCurrentModelFilePath() string {
	return filepath.Join(osenv.JujuXDGDataHome(), CurrentModelFilename)
}

func getCurrentControllerFilePath() string {
	return filepath.Join(osenv.JujuXDGDataHome(), CurrentControllerFilename)
}

// ReadCurrentModel reads the file $JUJU_DATA/current-model and
// return the value stored there.  If the file doesn't exist an empty string
// is returned and no error.
func ReadCurrentModel() (string, error) {
	lock, err := acquireEnvironmentLock("read current-model")
	if err != nil {
		return "", errors.Trace(err)
	}
	defer lock.Unlock()

	current, err := ioutil.ReadFile(getCurrentModelFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", errors.Trace(err)
	}
	return strings.TrimSpace(string(current)), nil
}

// ReadCurrentController reads the file $JUJU_DATA/current-controller and
// return the value stored there. If the file doesn't exist an empty string is
// returned and no error.
func ReadCurrentController() (string, error) {
	lock, err := acquireEnvironmentLock("read current-controller")
	if err != nil {
		return "", errors.Trace(err)
	}
	defer lock.Unlock()

	current, err := ioutil.ReadFile(getCurrentControllerFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", errors.Trace(err)
	}
	return strings.TrimSpace(string(current)), nil
}

// WriteCurrentModel writes the envName to the file
// $JUJU_DATA/current-model file.
func WriteCurrentModel(envName string) error {
	lock, err := acquireEnvironmentLock("write current-model")
	if err != nil {
		return errors.Trace(err)
	}
	defer lock.Unlock()

	path := getCurrentModelFilePath()
	err = ioutil.WriteFile(path, []byte(envName+"\n"), 0644)
	if err != nil {
		return errors.Errorf("unable to write to the model file: %q, %s", path, err)
	}
	// If there is a current controller file, remove it.
	if err := os.Remove(getCurrentControllerFilePath()); err != nil && !os.IsNotExist(err) {
		logger.Debugf("removing the current model file due to %s", err)
		// Best attempt to remove the file we just wrote.
		os.Remove(path)
		return err
	}
	return nil
}

// WriteCurrentController writes the controllerName to the file
// $JUJU_DATA/current-controller file.
func WriteCurrentController(controllerName string) error {
	lock, err := acquireEnvironmentLock("write current-controller")
	if err != nil {
		return errors.Trace(err)
	}
	defer lock.Unlock()

	path := getCurrentControllerFilePath()
	err = ioutil.WriteFile(path, []byte(controllerName+"\n"), 0644)
	if err != nil {
		return errors.Errorf("unable to write to the controller file: %q, %s", path, err)
	}
	// If there is a current environment file, remove it.
	if err := os.Remove(getCurrentModelFilePath()); err != nil && !os.IsNotExist(err) {
		logger.Debugf("removing the current controller file due to %s", err)
		// Best attempt to remove the file we just wrote.
		os.Remove(path)
		return err
	}
	return nil
}

func acquireEnvironmentLock(operation string) (*fslock.Lock, error) {
	// NOTE: any reading or writing from the directory should be done with a
	// fslock to make sure we have a consistent read or write.  Also worth
	// noting, we should use a very short timeout.
	lock, err := fslock.NewLock(osenv.JujuXDGDataHome(), lockName, fslock.Defaults())
	if err != nil {
		return nil, errors.Trace(err)
	}
	err = lock.LockWithTimeout(lockTimeout, operation)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return lock, nil
}

// CurrentConnectionName looks at both the current environment file
// and the current controller file to determine which is active.
// The name of the current model or controller is returned along with
// a boolean to express whether the name refers to a controller or environment.
func CurrentConnectionName() (name string, is_controller bool, err error) {
	currentEnv, err := ReadCurrentModel()
	if err != nil {
		return "", false, errors.Trace(err)
	} else if currentEnv != "" {
		return currentEnv, false, nil
	}

	currentController, err := ReadCurrentController()
	if err != nil {
		return "", false, errors.Trace(err)
	} else if currentController != "" {
		return currentController, true, nil
	}

	return "", false, nil
}

func currentName() (string, error) {
	name, isController, err := CurrentConnectionName()
	if err != nil {
		return "", errors.Trace(err)
	}
	if isController {
		name = name + controllerSuffix
	}
	if name != "" {
		name += " "
	}
	return name, nil
}

// SetCurrentModel writes out the current environment file and writes a
// standard message to the command context.
func SetCurrentModel(context *cmd.Context, modelName string) error {
	current, err := currentName()
	if err != nil {
		return errors.Trace(err)
	}
	err = WriteCurrentModel(modelName)
	if err != nil {
		return errors.Trace(err)
	}
	context.Infof("%s-> %s", current, modelName)
	return nil
}

// SetCurrentController writes out the current controller file and writes a standard
// message to the command context.
func SetCurrentController(context *cmd.Context, controllerName string) error {
	current, err := currentName()
	if err != nil {
		return errors.Trace(err)
	}
	err = WriteCurrentController(controllerName)
	if err != nil {
		return errors.Trace(err)
	}
	context.Infof("%s-> %s%s", current, controllerName, controllerSuffix)
	return nil
}
