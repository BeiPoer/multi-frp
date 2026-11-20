# MultiFRP

MultiFRP 用一台公网服务器管理多台 Windows 设备的 TCP 隧道。服务端和 EXE 都使用官方 FRP `v0.72.0`。管理员发放一次性邀请码，设备绑定后提交隧道申请。后台批准并分配端口后，EXE 显示可复制的 `域名:端口`。

## 部署服务端

1. 在 Linux 公网服务器上准备 Docker Engine、Docker Compose v2 和 Nginx。把管理域名的 DNS 记录指向这台服务器。若使用单独的公网访问域名，也将其指向这台服务器。
2. 将 `.env.example` 复制为 `.env`，填写 `MULTIFRP_HOST` 和 `MULTIFRP_ADMIN_PASSWORD`。管理域名填写裸域名，例如 `manage.example.com`。密码长度至少为 16 字节，可在 `.env` 里用单引号保留 `$` 等字符。公网访问域名留空时使用管理域名，部署后可在网页后台修改。
3. 按下文配置 Nginx 的 HTTPS 反代。在防火墙和云安全组放行 TCP `443` 和实际使用的公网隧道端口，默认端口池为 `30000–30099`。如果 Nginx 用 `80` 做 HTTP 跳转或证书验证，也放行该端口。
4. 在项目目录执行：

   ```sh
   docker compose pull
   docker compose up -d
   docker compose ps
   ```

5. 打开 `https://你的管理域名`，使用 `.env` 中的管理员账号登录。默认用户名为 `admin`。

后台的「生成授权码」可填写备注。授权码列表保留未使用、已使用和已过期的记录，设备绑定后会显示所用的授权码及备注。在授权码列表或设备列表修改备注后，两处都会更新。升级前已绑定的设备缺少授权码关联。旧版未使用的码可继续绑定，绑定后才会在后台显示完整码。

审批隧道时可以在 `MULTIFRP_PORT_FIRST` 到 `MULTIFRP_PORT_LAST` 之间指定公网端口，留空会自动分配。已批准的隧道可通过「改端口」调整。启用中的隧道更换端口后，旧入口及其连接会关闭，EXE 会同步新地址。修改停用隧道时先保存端口，启用时再检查宿主机是否可监听。

Docker 卷 `server-data` 保存设备授权、端口分配、后台域名和近 8 天的流量样本。常规重建复用原卷。扩充端口范围时保留已有分配所在的端口。

项目使用 `compose.yaml`（Docker Compose 默认识别），服务端直接拉取 `ghcr.io/beipoer/multi-frp:latest`，无需在部署机器上编译。更新时再次执行上面的 `pull` 和 `up -d`。要固定版本，在 `.env` 中设置 `MULTIFRP_IMAGE_TAG=v1.0.0`，对应标签必须已发布。镜像目前支持 Linux x64。

首次发布后，如需免登录拉取，请在 GitHub 仓库关联的 `multi-frp` Package 设置中将可见性改为 Public；若保留私有，则在部署机器上先使用有 `read:packages` 权限的凭据执行 `docker login ghcr.io`。

### 配置 Nginx

在同一台宿主机的 Nginx 上，为 `MULTIFRP_HOST` 配置 HTTPS 站点和受信任的证书。MultiFRP 的 HTTP 监听地址由 `.env` 中的 `MULTIFRP_HTTP_BIND` 控制，默认是 `127.0.0.1:8080`。例如端口 `8080` 已被占用时，可在 `.env` 中设置 `MULTIFRP_HTTP_BIND=127.0.0.1:18080`，执行 `docker compose up -d` 应用，再把 Nginx 中两处 `proxy_pass` 都改为 `http://127.0.0.1:18080`。Compose 健康检查会自动使用新的端口。

把以下 `location` 配置放进该域名监听 `443` 的 `server` 块。管理后台与 EXE 共用 `443`，其中 `/~!frp` 是 EXE 的 WebSocket 接入路径。

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
}

location = /~!frp {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 120s;
}
```

Nginx 如果也在 Docker 中运行，需要使用 `host` 网络才能访问上述回环地址。执行 `nginx -t` 检查后重载配置。可在服务器上请求 Nginx 上游地址的 `/healthz`，正常返回 `true`。

从旧的内置 HTTPS 服务迁移时，先准备好 Nginx 站点，再在原项目目录执行 `docker compose up -d --remove-orphans`。Compose 会清理旧版 `https` 容器并继续使用原有的 `server-data` 卷。若旧容器占着 `80` 或 `443`，等它退出后再启动或重载 Nginx。

## GitHub Actions 发布

`.github/workflows/publish.yml` 使用仓库自带的 `GITHUB_TOKEN`，无需配置 Docker Hub 账号或额外发布 Token。仓库需允许 Actions 运行及工作流申请 `packages: write` 和 `contents: write` 权限。

- 推送 `main`：运行 Go 测试，构建并推送 `latest` 和 `sha-<短提交号>` 镜像。
- 推送 `v*` 标签：测试并发布同名镜像（例如 `v1.0.0`），然后复用 `build-client.ps1` 构建 Windows x64 EXE，创建同名 GitHub Release 并上传 `MultiFRP-windows-amd64.exe`。重跑时更新已有 Release 的同名附件。
- Actions 页可手动运行 `Publish`。选择 `main` 更新 `latest`；选择已有 `v*` 标签可重新执行该版本发布。标签发布不覆盖 `latest`。

提交并推送代码后，发布版本示例：

```sh
git tag v1.0.0
git push origin v1.0.0
```

在仓库的 Actions 页面确认 `Publish` 成功后，从 [Releases](https://github.com/BeiPoer/multi-frp/releases) 下载 EXE。

## 使用 Windows 客户端

1. 从 [Releases](https://github.com/BeiPoer/multi-frp/releases) 下载并打开 `MultiFRP-windows-amd64.exe`，点击「绑定设备」，输入管理端地址和后台生成的一次性邀请码。首次安装 Windows 后台服务时需要本机管理员批准 UAC。一个邀请码可使用一次，有效期 24 小时。
2. 点击「申请 TCP 隧道」，填写名称、本机地址和本机端口。后台批准后，EXE 会自动同步并尝试上线。实际在线后，可复制公网访问地址。
3. 勾选「开机自启动（Windows 登录前）」，由 Windows 在电脑开机时启动后台服务。配置和设备凭据随后台服务保存，待网络可用后恢复已批准且启用的隧道。被映射的本机程序也需要在登录前已经监听端口。
4. 点击窗口右上角的「×」隐藏到右下角托盘，点击托盘图标恢复窗口。右键选择「完全退出」时会停止后台服务并断开本机隧道，保留下次开机的自启动设置。取消勾选自启动会改为手动启动，当前服务会继续运行。

EXE 首次获准安装时复制到 `%ProgramFiles%\MultiFRP\MultiFRP.exe`。设备绑定和本机启停设置保存在同目录下受限的 `data` 子目录。构建产物为 Windows x64，源码可用 Go 1.26 和 PowerShell 重新生成：

```powershell
.\build-client.ps1
```

## 统计口径

后台展示每条隧道以公网入口为视角的双向 TCP 字节量。当前带宽每约 5 秒刷新，历史流量按分钟写入持久卷，详情提供 24 小时和 7 天的区间汇总与平均带宽曲线。后台停用单条隧道时关闭其现有公网 TCP 连接，同时拒绝新的转发。已分配的端口在停用后保留，重新启用可继续使用原地址。

当前公网入口采用通用 TCP 域名加端口。FRP 服务端校验设备身份、隧道 ID 和公网端口，客户端进程根据已审批配置连接本机地址。本机地址属于客户端运行配置。

## 验收

- 两台设备分别申请并开通隧道。让第一条保持 TCP 长连接传输，确认后台双向带宽和流量在连接关闭前就开始更新。停用第一条，确认其连接断开且第二条仍可访问。
- 再次执行 `docker compose pull` 和 `docker compose up -d`，确认设备绑定、域名、隧道配置和历史流量仍可查看。
- 在目标 Windows 上启用自启动后重启，停在密码输入界面。从另一台机器访问由该 Windows 设备映射的、已经启动的本机服务。登录后打开 EXE，确认「×」保留托盘和隧道，「完全退出」断开隧道。

## 升级到按需监听版本（v1.0.1）

服务端使用 Linux host 网络，端口池只规定可分配的公网端口范围，不再生成整段 Docker 端口映射。范围可设置为 `30000–40000`，允许范围为 `20000–65535`；只有已批准且启用的隧道才监听公网端口。停用或撤销会关闭监听与现有连接，保留分配记录，重启后恢复已启用的隧道。审批或重新启用时遇到占用会返回明确错误，保持原状态；重启恢复时遇到占用会报错退出，请先释放对应端口。

更新时同时更新仓库里的 `compose.yaml` 和服务端镜像。在保留 `.env` 和原有命名卷的前提下，在原项目目录执行：

```sh
git pull --ff-only
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
```

切换期间已有隧道会短暂断开。保留 `server-data` 卷即可保留授权和历史数据。检查云安全组及宿主机防火墙已放行 HTTPS 和实际使用的隧道端口。

管理 API 由同一宿主机上的 Nginx 转发，后端监听地址见 `MULTIFRP_HTTP_BIND`。FRP 控制端口 7000、状态端口 7500 和内部转发端口 12000–19999 仅监听回环地址。内部转发端口按需分配，与公网端口池互不重叠。每条启用过的隧道在本次服务运行期间保留一个内部端口编号，最多 8000 个，实际可用数取决于宿主机占用情况。预留编号不会创建监听，FRP 仅在客户端上线时监听内部端口。
