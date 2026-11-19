# MultiFRP

MultiFRP 用一台公网服务器管理多台 Windows 设备的 TCP 隧道。服务端和 EXE 都使用官方 FRP `v0.72.0`。管理员发放一次性邀请码，设备绑定后提交隧道申请。后台批准并分配端口后，EXE 显示可复制的 `域名:端口`。

## 部署服务端

1. 在 Linux 公网服务器上准备 Docker Engine 和 Docker Compose v2。把管理域名的 DNS 记录指向这台服务器。若使用单独的公网访问域名，也将其指向这台服务器。
2. 将 `.env.example` 复制为 `.env`，填写 `MULTIFRP_HOST` 和 `MULTIFRP_ADMIN_PASSWORD`。管理域名填写裸域名，例如 `manage.example.com`。密码长度至少为 16 字节，可在 `.env` 里用单引号保留 `$` 等字符。公网访问域名留空时使用管理域名，部署后可在网页后台修改。
3. 在服务器防火墙和云安全组放行 TCP `80`、`443` 和配置的公网隧道端口范围，默认 `30000–30099`。服务端与客户端的管理 API 和 FRP 连接共用 HTTPS `443`，Caddy 为管理域名签发证书。
4. 在项目目录执行：

   ```sh
   docker compose pull
   docker compose up -d
   docker compose ps
   ```

5. 打开 `https://你的管理域名`，使用 `.env` 中的管理员账号登录。默认用户名为 `admin`。

Docker 卷 `server-data` 保存设备授权、端口分配、后台域名和近 8 天的流量样本，`https-data` 保存 Caddy 证书。常规重建复用原卷。扩充端口范围时保留已有分配所在的端口。

项目使用 `compose.yaml`（Docker Compose 默认识别），服务端直接拉取 `ghcr.io/beipoer/multi-frp:latest`，无需在部署机器上编译。更新时再次执行上面的 `pull` 和 `up -d`。要固定版本，在 `.env` 中设置 `MULTIFRP_IMAGE_TAG=v1.0.0`，对应标签必须已发布。镜像目前支持 Linux x64。

首次发布后，如需免登录拉取，请在 GitHub 仓库关联的 `multi-frp` Package 设置中将可见性改为 Public；若保留私有，则在部署机器上先使用有 `read:packages` 权限的凭据执行 `docker login ghcr.io`。

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

更新时必须同时更新仓库里的 `compose.yaml` 和 `deploy/Caddyfile`，不能只拉取新镜像。在原项目目录执行（保留 `.env` 和原有命名卷）：

```sh
git pull --ff-only
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
```

切换期间已有隧道会短暂断开。不要执行 `docker compose down -v`，以免删除授权与历史数据。云安全组及宿主机防火墙仍需放行 TCP 80、443 和实际使用的隧道端口；host 网络下须检查宿主机防火墙规则。

管理 API 默认只监听 `127.0.0.1:8080`，Caddy 在同一宿主机通过该地址反向代理；FRP 控制端口 7000、状态端口 7500 和内部转发端口 12000–19999 也仅监听回环地址。请避免其他服务占用这些端口以及 Caddy 的 80/443。内部转发端口按需分配，与公网端口池互不重叠；每条启用过的隧道在本次服务运行期间保留一个内部端口编号，最多 8000 个，实际可用数取决于宿主机占用情况。预留编号不会创建监听，FRP 仅在客户端上线时监听内部端口。
