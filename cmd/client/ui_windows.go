package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"multifrp/internal/control"
)

type tunnelTable struct {
	walk.TableModelBase
	tunnels []control.Tunnel
}

func (t *tunnelTable) RowCount() int { return len(t.tunnels) }

func (t *tunnelTable) Value(row, column int) any {
	item := t.tunnels[row]
	switch column {
	case 0:
		return item.Name
	case 1:
		return net.JoinHostPort(item.LocalIP, strconv.Itoa(item.LocalPort))
	case 2:
		return item.Address
	case 3:
		return tunnelLabel(item)
	}
	return ""
}

func tunnelLabel(t control.Tunnel) string {
	switch t.State {
	case "pending":
		return "待审批"
	case "rejected":
		return "已拒绝"
	case "paused":
		return "本机已停止"
	}
	if !t.Enabled {
		return "后台已停用"
	}
	if t.Online {
		return "在线"
	}
	return "等待连接"
}

type windowUI struct {
	window                                  *walk.MainWindow
	table                                   *walk.TableView
	auto                                    *walk.CheckBox
	status, device                          *walk.Label
	start, bind, submit, toggle, copy, logs *walk.PushButton
	model                                   *tunnelTable
	last                                    pipeResponse
	busy, applying, quitting                bool
}

func runWindow() error {
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Local\MultiFRP.Client`))
	if mutex != 0 {
		defer windows.CloseHandle(mutex)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		walk.MsgBox(nil, "MultiFRP", "客户端已在运行，可从右下角托盘打开。", walk.MsgBoxIconInformation)
		return nil
	}
	if err != nil {
		return err
	}
	u := &windowUI{model: &tunnelTable{}}
	if err := (MainWindow{
		AssignTo: &u.window, Title: "MultiFRP", Size: Size{Width: 885, Height: 500}, MinSize: Size{Width: 700, Height: 410},
		Font:   Font{Family: "Microsoft YaHei UI", PointSize: 9},
		Layout: VBox{Margins: Margins{Left: 18, Right: 18, Top: 16, Bottom: 14}, Spacing: 10},
		Children: []Widget{
			Composite{Layout: HBox{MarginsZero: true, Spacing: 10}, Children: []Widget{
				Label{Text: "MultiFRP", Font: Font{Family: "Microsoft YaHei UI", PointSize: 20, Bold: true}}, HSpacer{},
				CheckBox{AssignTo: &u.auto, Text: "开机自启动（Windows 登录前）", OnCheckedChanged: u.changeAuto},
			}},
			Label{AssignTo: &u.status, Text: "正在读取后台服务…", MaxSize: Size{Width: 815}},
			Composite{Layout: HBox{MarginsZero: true, Spacing: 8}, Children: []Widget{
				PushButton{AssignTo: &u.start, Text: "启动后台服务", OnClicked: u.startService},
				PushButton{AssignTo: &u.bind, Text: "绑定设备", OnClicked: u.bindDevice},
				Label{AssignTo: &u.device, Text: "设备未绑定"}, HSpacer{},
				PushButton{AssignTo: &u.submit, Text: "申请 TCP 隧道", OnClicked: u.submitTunnel},
			}},
			TableView{AssignTo: &u.table, Model: u.model, StretchFactor: 1, LastColumnStretched: true, NotSortableByHeaderClick: true,
				AlternatingRowBG: true, OnCurrentIndexChanged: u.updateButtons, OnItemActivated: u.copyAddress,
				Columns: []TableViewColumn{{Title: "隧道名称", Width: 195}, {Title: "本机地址", Width: 165}, {Title: "公网访问地址", Width: 230}, {Title: "状态", Width: 125}}},
			Composite{Layout: HBox{MarginsZero: true, Spacing: 8}, Children: []Widget{
				PushButton{AssignTo: &u.copy, Text: "复制公网地址", OnClicked: u.copyAddress},
				PushButton{AssignTo: &u.toggle, Text: "停止所选隧道", OnClicked: u.toggleTunnel},
				HSpacer{}, PushButton{AssignTo: &u.logs, Text: "查看日志", OnClicked: u.openLogs},
			}},
		},
	}).Create(); err != nil {
		return err
	}
	defer u.window.Dispose()
	icon, err := makeIcon()
	if err != nil {
		return err
	}
	defer icon.Dispose()
	_ = u.window.SetIcon(icon)
	tray, err := walk.NewNotifyIcon(u.window)
	if err != nil {
		return err
	}
	defer tray.Dispose()
	if err := tray.SetIcon(icon); err != nil {
		return err
	}
	if err := tray.SetToolTip("MultiFRP"); err != nil {
		return err
	}
	show := func() { u.window.Show(); _ = u.window.BringToTop() }
	tray.MouseUp().Attach(func(_, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			show()
		}
	})
	for _, item := range []struct {
		label string
		run   func()
	}{{"打开主窗口", show}, {"完全退出", u.exit}} {
		action := walk.NewAction()
		_ = action.SetText(item.label)
		action.Triggered().Attach(item.run)
		if err := tray.ContextMenu().Actions().Add(action); err != nil {
			return err
		}
	}
	if err := tray.SetVisible(true); err != nil {
		return err
	}
	u.window.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if !u.quitting {
			*canceled = true
			u.window.SetVisible(false)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.refreshLoop(ctx)
	u.window.Show()
	u.window.Run()
	return nil
}

func makeIcon() (*walk.Icon, error) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	fill := func(x1, y1, x2, y2 int, c color.Color) {
		draw.Draw(img, image.Rect(x1, y1, x2, y2), &image.Uniform{C: c}, image.Point{}, draw.Src)
	}
	blue := color.RGBA{R: 32, G: 92, B: 212, A: 255}
	fill(3, 1, 29, 31, blue)
	fill(1, 3, 31, 29, blue)
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	fill(7, 7, 14, 14, white)
	fill(18, 18, 25, 25, white)
	fill(11, 11, 21, 14, white)
	fill(18, 12, 21, 21, white)
	return walk.NewIconFromImage(img)
}

func (u *windowUI) refreshLoop(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		refresh, cancel := context.WithTimeout(ctx, time.Second)
		state, automatic, err := installedService()
		var result pipeResponse
		if err == nil && state == svc.Running {
			result, err = call(refresh, pipeRequest{Action: "status"})
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		u.window.Synchronize(func() {
			if !u.busy && !u.quitting {
				u.render(state, automatic, result, err)
			}
		})
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (u *windowUI) render(state svc.State, automatic bool, result pipeResponse, err error) {
	u.applying = true
	u.auto.SetChecked(automatic)
	u.applying = false
	u.last = result
	running := state == svc.Running && err == nil
	u.start.SetEnabled(!running)
	u.bind.SetEnabled(!running || result.DeviceID == "" || result.Status == "设备授权已撤销")
	u.submit.SetEnabled(running && result.DeviceID != "" && result.Status != "设备授权已撤销")
	u.logs.SetEnabled(running)
	switch {
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		_ = u.status.SetText("后台服务尚未安装，绑定设备时会请求管理员授权")
	case err != nil:
		_ = u.status.SetText("无法读取后台服务 · " + err.Error())
	case !running:
		_ = u.status.SetText("后台服务已停止")
	default:
		_ = u.status.SetText(result.Status)
	}
	if result.DeviceID == "" {
		_ = u.device.SetText("设备未连接")
	} else {
		_ = u.device.SetText(result.Origin)
	}
	if !slices.Equal(u.model.tunnels, result.Tunnels) {
		selected, _ := u.selected()
		u.model.tunnels = result.Tunnels
		u.model.PublishRowsReset()
		for i, t := range result.Tunnels {
			if t.ID == selected.ID {
				_ = u.table.SetCurrentIndex(i)
				break
			}
		}
	}
	u.updateButtons()
}

func (u *windowUI) selected() (control.Tunnel, bool) {
	index := u.table.CurrentIndex()
	if index < 0 || index >= len(u.model.tunnels) {
		return control.Tunnel{}, false
	}
	return u.model.tunnels[index], true
}

func (u *windowUI) updateButtons() {
	if u.table == nil || u.copy == nil || u.toggle == nil {
		return
	}
	t, ok := u.selected()
	u.copy.SetEnabled(ok && t.Address != "")
	u.toggle.SetEnabled(ok && t.Enabled && (t.State == "approved" || t.State == "paused"))
	label := "停止所选隧道"
	if t.State == "paused" {
		label = "启动所选隧道"
	}
	_ = u.toggle.SetText(label)
}

func (u *windowUI) copyAddress() {
	if t, ok := u.selected(); ok && t.Address != "" {
		if err := walk.Clipboard().SetText(t.Address); err != nil {
			u.message(err)
		}
	}
}

func (u *windowUI) toggleTunnel() {
	if t, ok := u.selected(); ok {
		u.request(pipeRequest{Action: "toggle", Tunnel: t}, nil)
	}
}

func (u *windowUI) message(err error) {
	if err != nil {
		walk.MsgBox(u.window, "MultiFRP", err.Error(), walk.MsgBoxIconError)
	}
}

func (u *windowUI) work(task func(context.Context) (pipeResponse, error), done func(pipeResponse)) {
	if u.busy {
		return
	}
	u.busy = true
	_ = u.status.SetText("正在处理…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		result, err := task(ctx)
		u.window.Synchronize(func() {
			u.busy = false
			if err != nil {
				u.message(err)
				return
			}
			if done != nil {
				done(result)
			}
		})
	}()
}

func (u *windowUI) request(request pipeRequest, done func(pipeResponse)) {
	u.work(func(ctx context.Context) (pipeResponse, error) { return call(ctx, request) }, done)
}

func (u *windowUI) configure(mode int, done func(pipeResponse)) {
	if u.busy {
		return
	}
	if err := elevate(windows.Handle(u.window.Handle()), mode); err != nil {
		u.message(err)
		return
	}
	u.work(func(ctx context.Context) (pipeResponse, error) {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			state, auto, err := installedService()
			if err == nil {
				if mode == manualStartMode && !auto {
					return pipeResponse{}, nil
				}
				if state == svc.Running && (mode == keepStartMode || auto) {
					return call(ctx, pipeRequest{Action: "status"})
				}
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return pipeResponse{}, errors.New("未能完成后台服务设置，请检查管理员授权")
			}
		}
	}, done)
}

func (u *windowUI) changeAuto() {
	if u.applying {
		return
	}
	mode := manualStartMode
	if u.auto.Checked() {
		mode = autoStartMode
	}
	u.configure(mode, nil)
}

func (u *windowUI) startService() { u.configure(keepStartMode, nil) }

func (u *windowUI) bindDevice() {
	if u.busy {
		return
	}
	if state, _, err := installedService(); err != nil || state != svc.Running {
		u.configure(keepStartMode, func(result pipeResponse) { u.last = result; u.bindDevice() })
		return
	}
	name, _ := os.Hostname()
	values, ok := u.input("绑定设备", []field{{"管理端地址", u.last.Origin}, {"一次性邀请码", ""}, {"设备名称", name}})
	if ok {
		u.request(pipeRequest{Action: "enroll", Origin: values[0], Code: values[1], Name: values[2]}, nil)
	}
}

func (u *windowUI) submitTunnel() {
	values, ok := u.input("申请 TCP 隧道", []field{{"隧道名称", ""}, {"本机 IP", "127.0.0.1"}, {"本机端口", ""}})
	if !ok {
		return
	}
	port, err := strconv.Atoi(values[2])
	if err != nil || port < 1 || port > 65535 {
		u.message(errors.New("请输入 1–65535 的本机端口"))
		return
	}
	u.request(pipeRequest{Action: "submit", Tunnel: control.Tunnel{Name: values[0], LocalIP: values[1], LocalPort: port}}, nil)
}

type field struct{ label, initial string }

func (u *windowUI) input(title string, fields []field) ([]string, bool) {
	var dlg *walk.Dialog
	var accept, cancel *walk.PushButton
	inputs := make([]*walk.LineEdit, len(fields))
	values := make([]string, len(fields))
	widgets := []Widget{}
	for i, item := range fields {
		widgets = append(widgets, Label{Text: item.label}, LineEdit{AssignTo: &inputs[i], Text: item.initial, MinSize: Size{Width: 355}})
	}
	widgets = append(widgets, Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
		HSpacer{}, PushButton{AssignTo: &accept, Text: "确认", OnClicked: func() {
			for i, input := range inputs {
				values[i] = strings.TrimSpace(input.Text())
				if values[i] == "" {
					u.message(fmt.Errorf("请输入%s", fields[i].label))
					return
				}
			}
			dlg.Accept()
		}}, PushButton{AssignTo: &cancel, Text: "取消", OnClicked: func() { dlg.Cancel() }},
	}})
	result, err := (Dialog{AssignTo: &dlg, Title: title, DefaultButton: &accept, CancelButton: &cancel, Layout: VBox{}, Children: widgets}).Run(u.window)
	if err != nil {
		u.message(err)
	}
	return values, err == nil && result == walk.DlgCmdOK
}

func (u *windowUI) openLogs() {
	u.request(pipeRequest{Action: "logs"}, func(result pipeResponse) {
		_, err := (Dialog{Title: "MultiFRP 日志", Size: Size{Width: 740, Height: 425}, MinSize: Size{Width: 480, Height: 250}, Layout: VBox{},
			Children: []Widget{TextEdit{Text: result.Log, ReadOnly: true, VScroll: true, StretchFactor: 1}},
		}).Run(u.window)
		u.message(err)
	})
}

func (u *windowUI) exit() {
	u.work(func(ctx context.Context) (pipeResponse, error) {
		state, _, err := installedService()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || (err == nil && state == svc.Stopped) {
			return pipeResponse{}, nil
		}
		if err != nil {
			return pipeResponse{}, err
		}
		if state == svc.Running {
			_, quitErr := call(ctx, pipeRequest{Action: "quit"})
			if quitErr != nil && !errors.Is(quitErr, io.EOF) && !errors.Is(quitErr, windows.ERROR_BROKEN_PIPE) {
				return pipeResponse{}, quitErr
			}
		}
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			if state, _, err := installedService(); err == nil && state == svc.Stopped {
				return pipeResponse{}, nil
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return pipeResponse{}, errors.New("后台服务尚未停止，请重试")
			}
		}
	}, func(pipeResponse) { u.quitting = true; walk.App().Exit(0) })
}
