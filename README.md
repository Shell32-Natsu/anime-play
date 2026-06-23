# anime-play

[![Build and push Docker image](https://github.com/Shell32-Natsu/anime-play/actions/workflows/docker.yml/badge.svg)](https://github.com/Shell32-Natsu/anime-play/actions/workflows/docker.yml)
[![GHCR](https://img.shields.io/badge/ghcr.io-shell32--natsu%2Fanime--play-blue?logo=docker)](https://github.com/Shell32-Natsu/anime-play/pkgs/container/anime-play)

把 OpenList 上散乱命名的本地番剧，对外**伪装成一个在线番剧播放站点**：弹幕播放器按它惯常的「在线站点」爬取流程（搜索 → 选条目 → 选集 → 取视频地址）就能播到本地视频，从而给本地番剧挂上弹幕。

本项目是为配合 [Animeko](https://github.com/open-ani/animeko) 使用而写的：在 Animeko 里把本服务添加为一个自定义（Selector）数据源，即可在 Animeko 中直接搜到、播放 OpenList 上的本地番剧并自动挂载弹幕。其他支持「自定义网页源 + CSS/XPath 选择器」的播放器同样适用。

特点：

- 不重命名、不移动任何本地文件；不查任何外部 API
- 数据源只用 OpenList 的 `/api/fs/list` + `/api/fs/get`，对远程网盘的实际触碰压到最低
- 番剧名靠手动映射（`/admin` 网页编辑，JSON 持久化，热重载）
- Go 单二进制，Docker 部署

---

## 快速开始（直接用 GHCR 镜像，推荐）

镜像已发布在 GHCR（linux/amd64 + linux/arm64），不需要 clone 仓库。新建一个目录，放两个文件：

`docker-compose.yml`：

```yaml
services:
  anime-play:
    image: ghcr.io/shell32-natsu/anime-play:latest
    container_name: anime-play
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data        # 映射文件持久化目录
    env_file:
      - .env
```

`.env`（完整可选项见 [.env.example](.env.example)）：

```bash
OPENLIST_BASE_URL=http://192.168.1.10:5244
OPENLIST_TOKEN=alist-xxxxxxxxxxxxxxxx
SCAN_ROOTS=/anime,/anime2
# 建议设置，保护 /admin 管理页
ADMIN_TOKEN=change-me
```

启动 / 停止：

```bash
docker compose up -d             # 启动（修改 .env 后也用它重建容器生效）
docker compose logs -f           # 查看日志（确认扫描到的条目数）
docker compose pull && docker compose up -d   # 升级到最新镜像
docker compose stop              # 停止容器（保留容器与数据）
docker compose down              # 停止并删除容器（./data 下的映射文件不会丢）
```

不用 Compose 的话，等价的 `docker run`：

```bash
docker run -d --name anime-play \
  -p 8080:8080 \
  -v ./data:/data \
  -e OPENLIST_BASE_URL=http://192.168.1.10:5244 \
  -e OPENLIST_TOKEN=alist-xxxxxxxxxxxxxxxx \
  -e SCAN_ROOTS=/anime,/anime2 \
  ghcr.io/shell32-natsu/anime-play:latest
```

### 从源码部署

```bash
git clone https://github.com/Shell32-Natsu/anime-play.git && cd anime-play
cp .env.example .env             # 编辑 .env，填上必填三项
# 仓库自带的 docker-compose.yml 默认拉 GHCR 镜像；要本地构建的话，
# 把其中的 build: . 取消注释后：
docker compose up -d --build
```

### 镜像 tag 说明

GitHub Actions（`.github/workflows/docker.yml`）在 push 到 `main` 或打 `v*` 标签时自动跑测试、构建多架构镜像并推送：

- `ghcr.io/shell32-natsu/anime-play:latest` —— main 分支最新
- `ghcr.io/shell32-natsu/anime-play:main-<sha>` —— 按 commit 固定版本
- `ghcr.io/shell32-natsu/anime-play:1.2.3` / `1.2` —— 打 `v1.2.3` 标签时生成

### 启动后的初始配置

1. 打开 `http://<host>:8080/admin`，确认条目已扫出（新增番剧后可点「手动刷新索引」或访问 `/refresh`）
2. 在 `/admin` 给每个条目填上番剧名和别名（中文名 / 日文名 / 罗马音 / 简称，逗号分隔），保存即生效
3. 在弹幕播放器里把本服务配置成一个「在线站点」，选择器见下方 class 结构清单

## 配置（环境变量 / .env 文件）

配置既可以用真实环境变量注入，也可以写在 `.env` 文件里：

- **Docker Compose**：`docker-compose.yml` 通过 `env_file: .env` 把 `.env` 注入容器（推荐方式，见上）
- **裸跑二进制**：程序启动时会自动加载工作目录下的 `.env`（可用 `ENV_FILE=/path/to/.env` 指定其他路径）
- 优先级：**真实环境变量 > .env 文件 > 默认值**；`.env` 支持 `#` 注释、`export` 前缀、引号包裹的值

| 变量 | 必填 | 默认值 | 含义 |
|---|---|---|---|
| `OPENLIST_BASE_URL` | ✅ | — | OpenList 地址，例如 `http://192.168.1.10:5244` |
| `OPENLIST_TOKEN` | ✅ | — | OpenList 管理员 token（管理页「其他设置」获取） |
| `SCAN_ROOTS` | ✅ | — | 扫描的根路径，多个用逗号分隔，例如 `/anime,/anime2,/盘B/番剧` |
| `LISTEN_PORT` | | `8080` | 服务监听端口 |
| `ADMIN_TOKEN` | | （空） | `/admin`、`/refresh` 的访问口令。设置后浏览器访问会弹 Basic 认证（密码填 token，用户名任意），脚本调用可用请求头 `X-Admin-Token`。留空则管理端点不做鉴权，仅适合可信内网；`/search`、`/play` 始终无鉴权（供播放器爬取） |
| `MAPPING_FILE` | | `/data/mapping.json` | 映射文件路径，**必须放在挂载 volume 内**，否则容器重建后映射丢失 |
| `EPISODE_MAP_FILE` | | `/data/episodes.yaml` | 手动集数映射 YAML 路径（可选，文件不存在则全部走自动解析），见下方「手动集数映射」 |
| `REFRESH_INTERVAL` | | `30m` | 目录索引自动刷新间隔（Go duration 格式） |
| `RAWURL_CACHE_TTL` | | `1h` | 视频直链缓存 TTL，应**保守地小于** OpenList 签名有效期（S3 类驱动默认 4 小时） |
| `RAWURL_HOST_REWRITE` | | `auto` | 直链主机名自动替换（见下方「多入口访问」）。`auto` = 开启，`off` = 关闭 |
| `ENV_FILE` | | `.env` | 要加载的 .env 文件路径（仅裸跑二进制时有意义；默认文件不存在则跳过） |

## 配合 Animeko 使用

在 [Animeko](https://github.com/open-ani/animeko) 的「设置 → 数据源」里添加一个**自定义数据源（Selector）**，指向本服务即可：

- 站点地址 / 搜索地址：`http://<host>:8080/search?keyword=<关键词占位符>`（占位符按 Animeko 的格式填）
- 条目、集数、视频地址的选择器按下方「HTML class 结构清单」配置：条目链接 `.anime-link`、条目标题 `.anime-title`、集数链接 `.ep-link`（文本即「第 N 话」）、视频地址 `video.player` 的 `src`
- Animeko 用番剧的中文正式名搜索，所以请先在 `/admin` 里给条目配好番剧名映射，否则搜不到
- 弹幕由 Animeko 自己按番剧名 + 集数去弹幕站匹配，本服务不参与

## 对外端点与 HTML class 结构清单

播放器用 XPath / CSS 选择器从下列固定结构中提取数据。class 命名保持稳定，不会变更。

### `GET /search?keyword=xxx` —— 搜索页

关键词对每个条目的「映射 names 集合」做大小写不敏感的包含匹配；未映射条目用清洗后的目录名兜底。空 keyword 返回全部条目。

```html
<div class="search-result">
  <div class="anime-item">
    <a class="anime-link" href="/play?id={条目ID}">
      <span class="anime-title">番剧名</span>
    </a>
  </div>
  <!-- ...每个条目一个 .anime-item -->
</div>
```

| 要提取的内容 | CSS 选择器 | XPath |
|---|---|---|
| 条目块 | `.anime-item` | `//div[@class="anime-item"]` |
| 条目链接（相对地址） | `.anime-item .anime-link` 的 `href` | `//a[@class="anime-link"]/@href` |
| 条目标题 | `.anime-item .anime-title` 的文本 | `//span[@class="anime-title"]/text()` |

### `GET /play?id=xxx` 与 `GET /play?id=xxx&ep=N` —— 播放页

不带 `ep`：只返回集数列表（不取直链）。带 `ep=N`：在该集数链接上加 `ep-current` class，并**实时**调用 OpenList `/api/fs/get` 取签名直链写入 `<video>`。

```html
<h1 class="anime-title">番剧名</h1>
<video class="player" src="{实时取到的 raw_url}" controls></video>  <!-- 仅带 ep 参数时出现 -->
<div class="ep-list">
  <a class="ep-link" href="/play?id=xxx&ep=1">第 1 话</a>
  <a class="ep-link ep-current" href="/play?id=xxx&ep=2">第 2 话</a>
  <!-- 解析不到集数的视频（SP/OVA/特典/菜单等）排在末尾，链接文本为原文件名 -->
</div>
```

| 要提取的内容 | CSS 选择器 | XPath |
|---|---|---|
| 集数链接（相对地址） | `.ep-link` 的 `href` | `//a[contains(@class,"ep-link")]/@href` |
| 集数标题（「第 N 话」） | `.ep-link` 的文本 | `//a[contains(@class,"ep-link")]/text()` |
| 视频地址（签名直链） | `video.player` 的 `src` | `//video[@class="player"]/@src` |

`ep` 是该条目集数列表中的序号（1 开始）；链接文本「第 N 话」中的 N 才是从文件名解析出的真实集数（季中开播的番两者可能不同），播放器匹配弹幕用的是链接文本。

### `GET /refresh` —— 手动刷新索引

重新扫描所有 `SCAN_ROOTS`、重建条目索引。新增番剧后访问一次即可，平时按 `REFRESH_INTERVAL` 自动刷新。设置了 `ADMIN_TOKEN` 时此端点需要鉴权。

### `GET /admin` —— 映射管理界面

- 按扫描根分组列出所有条目，标明「已映射 / 未映射」与集数
- 直接在网页里给条目填写番剧名 + 别名（逗号分隔），保存即写入映射文件并立刻生效；留空保存 = 删除映射
- 未映射的条目搜索时用清洗后的目录名（去字幕组 / 画质标签）兜底，但建议尽快补全映射，否则播放器用中文正式名搜索通常搜不到
- 每个条目的「集数」列有「编辑」链接，进入 `/admin/episodes?id=...` 集数编辑页：逐个文件查看自动解析结果、填写手动集数（留空 = 用自动解析），保存即写入 `episodes.yaml` 并立刻生效（详见下方「手动集数映射」）
- 设置了 `ADMIN_TOKEN` 时需要鉴权（浏览器 Basic 认证密码填 token，或请求头 `X-Admin-Token`）
- 保存接口为 `POST /admin/save`（JSON：`{"dir": "...", "names": ["..."]}`）与 `POST /admin/episodes/save`（JSON：`{"dir": "...", "episodes": {"文件名": "集数", ...}}`），都仅接受当前索引中存在的 `dir`；若用脚本直接调用，需带请求头 `X-Requested-With: anime-play`（CSRF 防护）和 `Content-Type: application/json`

## 番剧名映射文件

路径由 `MAPPING_FILE` 指定（默认 `/data/mapping.json`），格式：

```json
{
  "entries": [
    {
      "dir": "/anime/[Lilith-Raws] Bocchi the Rock! [Baha][1080p]",
      "names": ["孤独摇滚", "ぼっち・ざ・ろっく！", "Bocchi the Rock"]
    }
  ]
}
```

- `dir`：条目对应的 OpenList 完整路径（唯一键，跨多个扫描根不会冲突）
- `names`：番剧名 + 所有别名，搜索时匹配这个集合

支持两种修改方式，都**无需重启**：

1. 在 `/admin` 网页里改 —— 保存时先更新内存、再原子写盘
2. 直接编辑文件（手动 / 其他工具）—— 服务用 fsnotify 监听文件所在目录，约 200ms 防抖后自动重读；若文件写到一半 / JSON 损坏，保留旧映射不受影响

## 手动集数映射（episodes.yaml，可选）

文件名实在太乱、自动解析不出集数或解析错误时，可以手动指定「集数 → 文件」。两种方式：

1. **网页编辑（推荐）**：`/admin` → 条目行的「编辑」→ 集数编辑页，逐个文件填手动集数后「保存全部」，立刻生效
2. **直接编辑 YAML 文件**：路径由 `EPISODE_MAP_FILE` 指定（默认 `/data/episodes.yaml`，模板见仓库根目录的 `episodes.example.yaml`）：

```yaml
entries:
  - dir: /anime/[Lilith-Raws] Bocchi the Rock! [Baha][1080p]
    episodes:
      1: "[Lilith-Raws] Bocchi the Rock! - 01 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4"
      12: "Bocchi Final (BDrip).mkv"
      12.5: "特典映像.mkv"
```

- `dir`：条目对应的 OpenList 完整路径（与 `/admin`、`mapping.json` 中的 dir 一致）
- `episodes`：键是集数（支持 `12.5` 这种小数），值是该条目文件夹下的视频文件名（不含路径）
- 手动指定的文件**优先于**文件名自动解析；未列出的文件仍走自动解析；指定的文件名必须真实存在于该文件夹
- 直接编辑文件保存后自动热重载（fsnotify + 防抖），只在内存里重建集数列表，**不会**重新请求 OpenList，也无需重启；YAML 写错时保留旧数据
- 注意：通过网页保存会重写整个 YAML 文件，手写的注释不会保留

## 多入口访问（内网 IP / Tailscale）

OpenList 返回的视频直链主机名是你在 `OPENLIST_BASE_URL` 里配的那个（通常是内网 IP）。如果你有时在内网、有时通过 Tailscale 访问同一台服务器，直链里的内网 IP 在 Tailscale 下是连不上的。

本服务默认开启**直链主机名自动替换**（`RAWURL_HOST_REWRITE=auto`）：渲染播放页时，凡是主机名等于 OpenList 主机名的直链，都把主机名替换成「客户端访问本服务所用的主机名」，端口保持不变——你用 `192.168.1.10:8080` 打开就得到内网直链，用 `100.x.y.z:8080`（或 ts.net 域名）打开就得到 Tailscale 直链。无需任何额外配置，也不用重启切换。

- 前提：OpenList 与本服务在同一台机器（或同样能通过这两个地址访问），且端口在两个网络下一致——Tailscale 场景天然满足
- 指向外部网盘 CDN 的直链（主机名不是 OpenList 的）不会被改动
- 如果你的部署不满足该前提（比如 OpenList 在另一台机器、或本服务挂在反向代理域名后），设 `RAWURL_HOST_REWRITE=off` 关闭

## 为什么视频直链是「实时取」的

OpenList 返回的 `raw_url` 是**带签名、有时效**的直链（S3 类驱动默认 4 小时，其他驱动随全局配置）。如果在扫描阶段就把直链存下来，等真正播放时签名很可能已过期。因此本服务：

- 扫描 / 搜索 / 列集数阶段**完全不调** `/api/fs/get`（零直链请求，只用 `fs/list` 的文件名元数据，且 `refresh:false` 不穿透 OpenList 缓存）
- 只有打开带 `ep=N` 的播放页那一刻，才对该集调用一次 `/api/fs/get`，把新鲜的签名直链写进 `<video src>`
- 取到的直链按 `RAWURL_CACHE_TTL` 在内存短期缓存：TTL 内重看同一集不再打网盘，过期后自动重取

这样既保证签名永远新鲜，又把对远程网盘（尤其有风控 / 限速的国内盘）的请求压到最低。

## 条目划分与集数规则

- 遍历每个扫描根，**「直接包含视频文件的那一层文件夹」= 一个条目（一季）**，自动覆盖 `季文件夹/视频` 与 `番剧名/季/视频` 两种结构
- 只保留视频扩展名（mkv / mp4 / ts / flv / webm / m3u8 / avi 等），排除图片、音频、字幕、nfo
- 集数从文件名解析（支持 `- 05`、`[05]`、`EP05`、`S01E05`、`第5话` 等常见格式），标题渲染为「第 N 话」；解析不对的可用「手动集数映射」覆盖
- SP / OVA / 特典等视频与正片一起列出；解析不到集数的排在末尾、标题用原文件名

## 本地开发

```bash
go test ./...
go build -o anime-play .

# 方式一：用 .env 文件（程序自动加载工作目录下的 .env）
cp .env.example .env   # 编辑后把 MAPPING_FILE 指向本地路径，如 ./data/mapping.json
./anime-play

# 方式二：直接传环境变量（优先级高于 .env）
OPENLIST_BASE_URL=http://192.168.1.10:5244 \
OPENLIST_TOKEN=alist-xxx \
SCAN_ROOTS=/anime \
MAPPING_FILE=./data/mapping.json \
./anime-play
```
