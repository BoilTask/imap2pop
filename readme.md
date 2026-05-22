# imap2pop

POP3 代理服务器，将飞书 IMAP 邮箱（imap.feishu.cn:993）转换为 POP3 协议，使传统邮件客户端可以通过 POP3 方式访问飞书邮箱。

## 功能

- 支持 POP3 标准命令：USER、PASS、STAT、LIST、RETR、DELE、UIDL、TOP、NOOP、RSET、QUIT、CAPA
- 通过 TLS 连接飞书 IMAP 服务器
- 每个客户端连接独立映射到一个 IMAP 会话
- 支持邮件删除（映射为 IMAP \Deleted 标记，QUIT 时 EXPUNGE）

## 使用

```bash
# 默认监听端口 1100
go run .

# 指定端口
go run . -port 110

# 开启 IMAP 协议详细日志
go run . -verbose
VERBOSE=1 go run .
```

编译为二进制：

```bash
go build -o imap2pop .
./imap2pop -port 110
./imap2pop -verbose   # 调试时开启
```

## ECS 部署

1. 编译：`go build -o imap2pop .`
2. 上传到 ECS
3. 创建 systemd 服务（参考 `imap2pop.service`）
4. 在安全组中开放 POP3 端口入方向 TCP

## 配置

| 参数 | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `-port` | `POP3_PORT` | 1100 | POP3 监听端口 |
| `-verbose` | `VERBOSE` | false | 输出 IMAP 协议交互详细日志 |

IMAP 目标固定为 `imap.feishu.cn:993`。

## 日志

默认日志仅输出关键事件和 POP3 命令，用 `[远程地址]` 标识 session：

```
[113.108.28.64:18102] connected
[113.108.28.64:18102] C USER tian.ouyang@feishu.cn
[113.108.28.64:18102] S +OK User accepted
[113.108.28.64:18102] LOGIN OK (tian.ouyang@feishu.cn)
[113.108.28.64:18102] SELECT OK, 513 messages
[113.108.28.64:18102] C STAT
[113.108.28.64:18102] S +OK 513 12345678
[113.108.28.64:18102] FETCH headers #1, 432 bytes
```

开启 `-verbose` 后额外输出每条 IMAP 收发记录（调试用，生产环境不建议开启）。