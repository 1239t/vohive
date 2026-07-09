package cscall

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/1239t/vohive/pkg/logger"
)

// AudioBridge PCM ↔ RTP 音频桥接器
// 通过 arecord/aplay 管道操作 ALSA 声卡，与 RTP 双向桥接
type AudioBridge struct {
	alsaDev    string                      // ALSA 设备名 (如 "hw:1,0")
	deviceID   string                      // 设备标识 (用于日志)
	rtpConn    *net.UDPConn                // RTP 本地 UDP 监听
	clientAddr atomic.Pointer[net.UDPAddr] // Linphone 的 RTP 地址

	// PCM 流控 (来自 +QPCMV URC)
	pcmReady atomic.Bool

	// 进程管理
	captureCmd  *exec.Cmd
	playbackCmd *exec.Cmd
	captureOut  io.ReadCloser
	playbackIn  io.WriteCloser

	// RTP 状态
	seqNum    uint16
	timestamp uint32
	ssrc      uint32

	// 生命周期
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewAudioBridge 创建音频桥接器
func NewAudioBridge(alsaDev, deviceID string) (*AudioBridge, error) {
	// 绑定随机 UDP 端口用于 RTP
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("绑定 RTP 端口失败: %w", err)
	}

	ab := &AudioBridge{
		alsaDev:  alsaDev,
		deviceID: deviceID,
		rtpConn:  conn,
		ssrc:     0x12345678, // 固定 SSRC
		stop:     make(chan struct{}),
	}
	ab.pcmReady.Store(false) // 初始为 false，必须等待 +QPCMV: 1 URC
	return ab, nil
}

// LocalPort 返回 RTP 本地监听端口 (用于 SDP)
func (ab *AudioBridge) LocalPort() int {
	return ab.rtpConn.LocalAddr().(*net.UDPAddr).Port
}

// SetClientAddr 设置 Linphone 的 RTP 地址 (从 SDP answer 解析)
func (ab *AudioBridge) SetClientAddr(ip string, port int) {
	addr := &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
	ab.clientAddr.Store(addr)
	logger.Info(fmt.Sprintf("[%s] AudioBridge: 设置 RTP 客户端地址 %s:%d", ab.deviceID, ip, port))
}

// SetPCMReady 设置 PCM 流控状态 (来自 +QPCMV URC)
func (ab *AudioBridge) SetPCMReady(ready bool) {
	ab.pcmReady.Store(ready)
}

// Start 启动双向桥接
func (ab *AudioBridge) Start() error {
	// 启动 ALSA 采集 (下行: EC20 → Linphone)
	// 每 40ms 输出 640 字节 (320 samples × 2B, 8kHz S16_LE mono)
	ab.captureCmd = exec.Command("arecord",
		"-D", ab.alsaDev,
		"-f", "S16_LE",
		"-r", "8000",
		"-c", "1",
		"-t", "raw",
		"--buffer-size", "640",
		"--period-size", "320",
	)

	var err error
	ab.captureOut, err = ab.captureCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 arecord 管道失败: %w", err)
	}
	if err := ab.captureCmd.Start(); err != nil {
		return fmt.Errorf("启动 arecord 失败: %w", err)
	}

	// 启动 ALSA 播放 (上行: Linphone → EC20)
	ab.playbackCmd = exec.Command("aplay",
		"-D", ab.alsaDev,
		"-f", "S16_LE",
		"-r", "8000",
		"-c", "1",
		"-t", "raw",
		"--buffer-size", "1600",
		"--period-size", "800",
	)

	ab.playbackIn, err = ab.playbackCmd.StdinPipe()
	if err != nil {
		ab.captureCmd.Process.Kill()
		return fmt.Errorf("创建 aplay 管道失败: %w", err)
	}
	if err := ab.playbackCmd.Start(); err != nil {
		ab.captureCmd.Process.Kill()
		return fmt.Errorf("启动 aplay 失败: %w", err)
	}

	logger.Info(fmt.Sprintf("[%s] AudioBridge: 已启动 (ALSA=%s, RTP=%d)", ab.deviceID, ab.alsaDev, ab.LocalPort()))

	// 启动桥接协程
	ab.wg.Add(2)
	go ab.loopCapture()  // 下行: ALSA → RTP
	go ab.loopPlayback() // 上行: RTP → ALSA

	// 启动静音保活协程：在 PCM 声道建立之前（最多 5 秒）每 20ms 发一个静音包
	// 防止 Linphone 因无 RTP 而超时挂断
	go func() {
		silencePCM := make([]byte, 320) // 320 字节 S16_LE 静音 = 160 个 μ-law 字节
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-ab.stop:
				return
			case <-deadline:
				return
			case <-ticker.C:
				clientAddr := ab.clientAddr.Load()
				if clientAddr == nil {
					continue
				}
				// 只在 pcmReady 为 false 时发静音（pcmReady=true 意味着真实音频已在流动）
				if ab.pcmReady.Load() {
					return
				}
				ulawPayload := EncodePCMToUlaw(silencePCM)
				rtpPacket := ab.buildRTPPacket(ulawPayload)
				ab.seqNum++
				ab.timestamp += 160
				ab.rtpConn.WriteToUDP(rtpPacket, clientAddr)
			}
		}
	}()

	return nil
}

// Stop 停止桥接并释放资源
func (ab *AudioBridge) Stop() {
	ab.stopOnce.Do(func() {
		close(ab.stop)

		// 关闭 UDP
		ab.rtpConn.Close()

		// 停止 ALSA 进程
		if ab.captureCmd != nil && ab.captureCmd.Process != nil {
			ab.captureCmd.Process.Kill()
		}
		if ab.playbackIn != nil {
			ab.playbackIn.Close()
		}
		if ab.playbackCmd != nil && ab.playbackCmd.Process != nil {
			ab.playbackCmd.Process.Kill()
		}

		// 启动一个后台 Goroutine 去等 Wait，并设置超时，防止 arecord 管道卡死
		go func() {
			waitCh := make(chan struct{})
			go func() {
				ab.wg.Wait()
				close(waitCh)
			}()

			select {
			case <-waitCh:
				logger.Info(fmt.Sprintf("[%s] AudioBridge: 正常停止结束", ab.deviceID))
			case <-time.After(2 * time.Second):
				logger.Warn(fmt.Sprintf("[%s] AudioBridge: 停止超时，强制放弃等待", ab.deviceID))
			}
		}()
	})
}

// loopCapture 下行: arecord stdout → G.711μ encode → RTP → Linphone
func (ab *AudioBridge) loopCapture() {
	defer ab.wg.Done()

	// 每次读取 640 字节 PCM (40ms, 320 samples)
	// 拆成 2 个 RTP 包 (每包 20ms, 160 samples → 160 bytes μ-law payload)
	pcmBuf := make([]byte, 640)

	for {
		select {
		case <-ab.stop:
			return
		default:
		}

		n, err := io.ReadFull(ab.captureOut, pcmBuf)
		if err != nil {
			select {
			case <-ab.stop:
				return
			default:
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					logger.Warn(fmt.Sprintf("[%s] AudioBridge: arecord 读取错误", ab.deviceID), "err", err)
				}
				return
			}
		}

		clientAddr := ab.clientAddr.Load()
		if clientAddr == nil {
			continue // 尚未获得客户端地址
		}

		// 拆成 2 个 20ms 帧
		for frame := 0; frame < 2 && frame*320 < n; frame++ {
			start := frame * 320
			end := start + 320
			if end > n {
				end = n
			}
			pcmFrame := pcmBuf[start:end]

			// PCM → G.711μ
			ulawPayload := EncodePCMToUlaw(pcmFrame)

			// 构造 RTP 包
			rtpPacket := ab.buildRTPPacket(ulawPayload)
			ab.seqNum++
			ab.timestamp += 160 // 20ms × 8000Hz

			// 发送
			ab.rtpConn.WriteToUDP(rtpPacket, clientAddr)
		}
	}
}

// loopPlayback 上行: Linphone RTP → G.711μ decode → aplay stdin
func (ab *AudioBridge) loopPlayback() {
	defer ab.wg.Done()

	buf := make([]byte, 1500) // MTU
	// 上行缓冲区: 凑够 1600 字节 PCM (100ms, 800 samples) 后写入
	pcmAccum := make([]byte, 0, 1600)

	for {
		select {
		case <-ab.stop:
			return
		default:
		}

		ab.rtpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, srcAddr, err := ab.rtpConn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// 超时但上行缓冲区有积累的数据，也写入
				if len(pcmAccum) > 0 {
					ab.playbackIn.Write(pcmAccum)
					pcmAccum = pcmAccum[:0]
				}
				continue
			}
			select {
			case <-ab.stop:
				return
			default:
				logger.Warn(fmt.Sprintf("[%s] AudioBridge: RTP 接收错误", ab.deviceID), "err", err)
				return
			}
		}

		if n < 12 {
			continue // 太短，不是有效 RTP
		}

		// 动态覆盖/学习客户端地址 (解决 NAT 对称端口变化问题)
		currAddr := ab.clientAddr.Load()
		if currAddr == nil || currAddr.String() != srcAddr.String() {
			ab.clientAddr.Store(srcAddr)
		}

		// 解析 RTP: 跳过头部，提取 payload
		headerLen := 12
		if n > headerLen {
			if !ab.pcmReady.Load() {
				continue // 模块（UAC）尚未就绪，暂不向上行注入避免爆音/阻塞
			}

			// 检查 RTP Payload Type (PT)
			pt := buf[1] & 0x7F
			if pt != 0 {
				logger.Warn(fmt.Sprintf("[%s] AudioBridge: 收到不支持的 RTP 载荷类型 %d, 将丢弃", ab.deviceID, pt))
				continue
			}

			payload := buf[headerLen:n]

			// G.711μ → PCM
			pcmData := DecodeUlawToPCM(payload)
			pcmAccum = append(pcmAccum, pcmData...)

			// 凑够 1600 字节 (100ms) 后写入 aplay
			if len(pcmAccum) >= 1600 {
				ab.playbackIn.Write(pcmAccum[:1600])
				// 保留余量
				remaining := make([]byte, len(pcmAccum)-1600)
				copy(remaining, pcmAccum[1600:])
				pcmAccum = remaining
			}
		}
	}
}

// buildRTPPacket 构造 RTP 数据包
// PT=0 (PCMU), 8000Hz
func (ab *AudioBridge) buildRTPPacket(payload []byte) []byte {
	pkt := make([]byte, 12+len(payload))

	// V=2, P=0, X=0, CC=0
	pkt[0] = 0x80
	// M=0, PT=0 (PCMU)
	pkt[1] = 0x00

	// Sequence Number
	binary.BigEndian.PutUint16(pkt[2:4], ab.seqNum)
	// Timestamp
	binary.BigEndian.PutUint32(pkt[4:8], ab.timestamp)
	// SSRC
	binary.BigEndian.PutUint32(pkt[8:12], ab.ssrc)

	// Payload
	copy(pkt[12:], payload)
	return pkt
}
