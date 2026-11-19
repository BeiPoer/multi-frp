package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func main() {
	var err error
	args := os.Args[1:]
	switch {
	case len(args) == 2 && args[0] == "--service":
		err = svc.Run(serviceName, serviceHandler{args[1]})
		if err != nil {
			os.Exit(1)
		}
	case len(args) == 3 && args[0] == "--configure":
		mode, parseErr := strconv.Atoi(args[1])
		if parseErr != nil {
			err = parseErr
		} else {
			err = configureService(mode, args[2])
		}
	case len(args) == 0:
		err = runWindow()
	default:
		err = errors.New("无法识别的启动参数")
	}
	if err != nil {
		walk.MsgBox(nil, "MultiFRP", err.Error(), walk.MsgBoxIconError)
	}
}

func installDir() string { return filepath.Join(os.Getenv("ProgramFiles"), "MultiFRP") }

const (
	keepStartMode   = -1
	manualStartMode = 0
	autoStartMode   = 1
)

func configureService(mode int, owner string) error {
	if mode < keepStartMode || mode > autoStartMode {
		return errors.New("无效的服务启动模式")
	}
	if _, err := windows.StringToSid(owner); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		service, err = installService(manager, owner)
	}
	if err != nil {
		return err
	}
	defer service.Close()
	config, err := service.Config()
	if err != nil {
		return err
	}
	if mode != keepStartMode {
		startType := uint32(mgr.StartManual)
		if mode == autoStartMode {
			startType = mgr.StartAutomatic
		}
		if config.StartType != startType {
			config.StartType = startType
			config.DelayedAutoStart = false
			if err := service.UpdateConfig(config); err != nil {
				return err
			}
		}
	}
	status, err := service.Query()
	if err != nil {
		return err
	}
	// Clearing automatic startup is independent of the current service state.
	if status.State == svc.Stopped && mode != manualStartMode {
		return service.Start()
	}
	return nil
}

func installService(manager *mgr.Mgr, owner string) (*mgr.Service, error) {
	base := installDir()
	if !filepath.IsAbs(base) {
		return nil, errors.New("无法识别 Program Files 目录")
	}
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	if err := restrictData(data); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	installed := filepath.Join(base, "MultiFRP.exe")
	if !equalPath(self, installed) {
		binary, err := os.Open(self)
		if err != nil {
			return nil, err
		}
		defer binary.Close()
		target, err := os.OpenFile(installed, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(target, binary)
		if err := errors.Join(copyErr, target.Close()); err != nil {
			return nil, err
		}
	}
	service, err := manager.CreateService(serviceName, installed, mgr.Config{
		DisplayName: "MultiFRP", Description: "Runs approved MultiFRP tunnels before Windows sign-in.",
		StartType: mgr.StartManual, ServiceStartName: `NT AUTHORITY\LocalService`,
	}, "--service", owner)
	if err != nil {
		return nil, err
	}
	actions := []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}
	if err := service.SetRecoveryActions(actions, 86400); err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}

func equalPath(a, b string) bool {
	first, err := os.Stat(a)
	if err != nil {
		return false
	}
	second, err := os.Stat(b)
	return err == nil && os.SameFile(first, second)
}

func restrictData(path string) error {
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;LS)")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func installedService() (svc.State, bool, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, false, err
	}
	defer windows.CloseServiceHandle(manager)
	name := windows.StringToUTF16Ptr(serviceName)
	handle, err := windows.OpenService(manager, name, windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, false, err
	}
	service := &mgr.Service{Name: serviceName, Handle: handle}
	defer service.Close()
	state, err := service.Query()
	if err != nil {
		return 0, false, err
	}
	config, err := service.Config()
	return state.State, config.StartType == mgr.StartAutomatic, err
}

func elevate(hwnd windows.Handle, mode int) error {
	identity, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := fmt.Sprintf("--configure %d %s", mode, identity.User.Sid.String())
	return windows.ShellExecute(hwnd, windows.StringToUTF16Ptr("runas"), windows.StringToUTF16Ptr(exe), windows.StringToUTF16Ptr(args), nil, 1)
}

func call(ctx context.Context, request pipeRequest) (pipeResponse, error) {
	state, _, err := installedService()
	if err != nil {
		return pipeResponse{}, err
	}
	if state != svc.Running {
		return pipeResponse{}, errors.New("后台服务未运行")
	}
	const access = windows.FILE_GENERIC_READ | windows.FILE_WRITE_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES
	conn, err := winio.DialPipeAccess(ctx, pipePath, access)
	if err != nil {
		return pipeResponse{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return pipeResponse{}, err
	}
	var response pipeResponse
	if err := json.NewDecoder(io.LimitReader(conn, 512<<10)).Decode(&response); err != nil {
		return pipeResponse{}, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}
