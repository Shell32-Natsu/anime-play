# anime-play

把 OpenList 上散乱命名的本地番剧，对外**伪装成一个在线番剧播放站点**：弹幕播放器按它惯常的「在线站点」爬取流程（搜索 → 选条目 → 选集 → 取视频地址）就能播到本地视频，从而给本地番剧挂上弹幕。

特点：

- 不重命名、不移动任何本地文件；不查任何外部 API
- 数据源只用 OpenList 的 `/api/fs/list` + `/api/fs/get`，对远程网盘的实际触碰压到最低
- 番剧名靠手动映射（`/admin` 网页编辑，JSON 持久化，热重载）
- Go 单二进制，Docker 部署

---

## 快速开始（Docker Compose，推荐）

1. 准备配置文件（compose 通过 `env_file` 把 `.env` 注入容器，`.env` 已在 .gitignore 里，不会被提交）：

```bash
cp .env.example .env
# 编辑 .env，填上 OPENLIST_BASE_URL / OPENLIST_TOKEN / SCAN_ROOTS，其余可保持默认
```

2. 启动：

```bash
docker compose up -d --build     # 首次或代码更新后（构建镜像并后台启动）
docker compose logs -f           # 查看运行日志（确认扫描到的条目数）
```

3. 停止 / 重启 / 卸载：

```bash
docker compose stop              # 停止容器（保留容器与数据）
docker compose start             # 再次启动
docker compose up -d             # 修改 .env 后用 up -d 重建容器使新配置生效
docker compose down              # 停止并删除容器（./data 下的 mapping.json 不会丢）
```

映射文件持久化在 `./data/mapping.json`（compose 中挂载到容器内 `/data`），删除容器、升级镜像都不影响已配置的映射。

### 不用 Compose 的 docker run 方式

```bash
docker build -t anime-play .

docker run -d --name anime-play \
  -p 8080:8080 \
  -v ./data:/data \
  -e OPENLIST_BASE_URL=http://192.168.1.10:5244 \
  -e OPENLIST_TOKEN=alist-xxxxxxxxxxxxxxxx \
  -e SCAN_ROOTS=/anime,/anime2,/盘B/番剧 \
  anime-play

# 停止 / 启动 / 删除
docker stop anime-play
docker start anime-play
docker rm -f anime-play
```

启动后：

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
| `MAPPING_FILE` | | `/data/mapping.json` | 映射文件路径，**必须放在挂载 volume 内**，否则容器重建后映射丢失 |
| `REFRESH_INTERVAL` | | `30m` | 目录索引自动刷新间隔（Go duration 格式） |
| `RAWURL_CACHE_TTL` | | `1h` | 视频直链缓存 TTL，应**保守地小于** OpenList 签名有效期（S3 类驱动默认 4 小时） |
| `ENV_FILE` | | `.env` | 要加载的 .env 文件路径（仅裸跑二进制时有意义；默认文件不存在则跳过） |

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

重新扫描所有 `SCAN_ROOTS`、重建条目索引。新增番剧后访问一次即可，平时按 `REFRESH_INTERVAL` 自动刷新。

### `GET /admin` —— 映射管理界面

- 按扫描根分组列出所有条目，标明「已映射 / 未映射」与集数
- 直接在网页里给条目填写番剧名 + 别名（逗号分隔），保存即写入映射文件并立刻生效；留空保存 = 删除映射
- 未映射的条目搜索时用清洗后的目录名（去字幕组 / 画质标签）兜底，但建议尽快补全映射，否则播放器用中文正式名搜索通常搜不到

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

## 为什么视频直链是「实时取」的

OpenList 返回的 `raw_url` 是**带签名、有时效**的直链（S3 类驱动默认 4 小时，其他驱动随全局配置）。如果在扫描阶段就把直链存下来，等真正播放时签名很可能已过期。因此本服务：

- 扫描 / 搜索 / 列集数阶段**完全不调** `/api/fs/get`（零直链请求，只用 `fs/list` 的文件名元数据，且 `refresh:false` 不穿透 OpenList 缓存）
- 只有打开带 `ep=N` 的播放页那一刻，才对该集调用一次 `/api/fs/get`，把新鲜的签名直链写进 `<video src>`
- 取到的直链按 `RAWURL_CACHE_TTL` 在内存短期缓存：TTL 内重看同一集不再打网盘，过期后自动重取

这样既保证签名永远新鲜，又把对远程网盘（尤其有风控 / 限速的国内盘）的请求压到最低。

## 条目划分与集数规则

- 遍历每个扫描根，**「直接包含视频文件的那一层文件夹」= 一个条目（一季）**，自动覆盖 `季文件夹/视频` 与 `番剧名/季/视频` 两种结构
- 只保留视频扩展名（mkv / mp4 / ts / flv / webm / m3u8 / avi 等），排除图片、音频、字幕、nfo
- 集数从文件名解析（支持 `- 05`、`[05]`、`EP05`、`S01E05`、`第5话` 等常见格式），标题渲染为「第 N 话」
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
