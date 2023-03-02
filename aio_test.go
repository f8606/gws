package gws

import (
	"bufio"
	"bytes"
	"github.com/lxzan/gws/internal"
	"github.com/stretchr/testify/assert"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 创建用于测试的对等连接
func testNewPeer(config *Upgrader) (server, client *Conn) {
	size := 4096
	s, c := Pipe()
	{
		brw := bufio.NewReadWriter(bufio.NewReaderSize(s, size), bufio.NewWriterSize(s, size))
		server = serveWebSocket(config, &Request{}, s, brw, config.EventHandler, config.CompressEnabled)
	}
	{
		brw := bufio.NewReadWriter(bufio.NewReaderSize(c, size), bufio.NewWriterSize(c, size))
		client = serveWebSocket(config, &Request{}, c, brw, new(BuiltinEventHandler), config.CompressEnabled)
	}
	return
}

func testCloneBytes(b []byte) []byte {
	p := make([]byte, len(b))
	copy(p, b)
	return p
}

// 模拟客户端写入
func testClientWrite(client *Conn, fin bool, opcode Opcode, payload []byte) error {
	if atomic.LoadUint32(&client.closed) == 1 {
		return internal.ErrConnClosed
	}
	err := doTestClientWrite(client, fin, opcode, payload)
	client.emitError(err)
	return err
}

func doTestClientWrite(client *Conn, fin bool, opcode Opcode, payload []byte) error {
	client.wmu.Lock()
	defer client.wmu.Unlock()

	var enableCompress = client.compressEnabled && opcode.IsDataFrame() && len(payload) >= client.config.CompressionThreshold
	if enableCompress {
		compressedContent, err := client.compressor.Compress(bytes.NewBuffer(payload))
		if err != nil {
			return internal.NewError(internal.CloseInternalServerErr, err)
		}
		payload = compressedContent.Bytes()
	}

	var header = frameHeader{}
	var n = len(payload)
	var headerLength = header.GenerateServerHeader(fin, enableCompress, opcode, n)

	header[1] += 128
	var key = internal.NewMaskKey()
	copy(header[headerLength:headerLength+4], key[:4])
	headerLength += 4

	internal.MaskXOR(payload, key[:4])
	if err := internal.WriteN(client.wbuf, header[:headerLength], headerLength); err != nil {
		return err
	}
	if err := internal.WriteN(client.wbuf, payload, n); err != nil {
		return err
	}
	return client.wbuf.Flush()
}

// 测试异步写入
func TestConn_WriteAsync(t *testing.T) {
	var as = assert.New(t)

	// 关闭压缩
	t.Run("plain text", func(t *testing.T) {
		var handler = new(webSocketMocker)
		var upgrader = NewUpgrader(func(c *Upgrader) {
			c.EventHandler = handler
		})
		server, client := testNewPeer(upgrader)

		var listA []string
		var listB []string
		var count = 128

		go func() {
			for i := 0; i < count; i++ {
				var n = internal.AlphabetNumeric.Intn(125)
				var message = internal.AlphabetNumeric.Generate(n)
				listA = append(listA, string(message))
				server.WriteAsync(OpcodeText, message)
			}
		}()

		var wg sync.WaitGroup
		wg.Add(count)

		go func() {
			for {
				var header = frameHeader{}
				_, err := client.conn.Read(header[:2])
				if err != nil {
					return
				}
				var payload = make([]byte, header.GetLengthCode())
				if _, err := client.conn.Read(payload); err != nil {
					return
				}
				listB = append(listB, string(payload))
				wg.Done()
			}
		}()

		wg.Wait()
		as.ElementsMatch(listA, listB)
	})

	// 开启压缩
	t.Run("compressed text", func(t *testing.T) {
		var handler = new(webSocketMocker)
		var upgrader = NewUpgrader(func(c *Upgrader) {
			c.EventHandler = handler
			c.CompressEnabled = true
			c.CompressionThreshold = 1
		})
		server, client := testNewPeer(upgrader)

		var listA []string
		var listB []string
		const count = 128

		go func() {
			for i := 0; i < count; i++ {
				var n = internal.AlphabetNumeric.Intn(1024)
				var message = internal.AlphabetNumeric.Generate(n)
				listA = append(listA, string(message))
				server.WriteAsync(OpcodeText, message)
			}
		}()

		var wg sync.WaitGroup
		wg.Add(count)

		go func() {
			for {
				var header = frameHeader{}
				length, err := header.Parse(client.rbuf)
				if err != nil {
					return
				}
				var payload = make([]byte, length)
				if _, err := client.rbuf.Read(payload); err != nil {
					return
				}
				if header.GetRSV1() {
					buf, err := client.decompressor.Decompress(bytes.NewBuffer(payload))
					if err != nil {
						return
					}
					payload = buf.Bytes()
				}
				listB = append(listB, string(payload))
				wg.Done()
			}
		}()

		wg.Wait()
		as.ElementsMatch(listA, listB)
	})

	// 往关闭的连接写数据
	t.Run("write to closed conn", func(t *testing.T) {
		var wg = sync.WaitGroup{}
		wg.Add(1)
		var handler = new(webSocketMocker)
		handler.onError = func(socket *Conn, err error) {
			as.Error(err)
			wg.Done()
		}
		var upgrader = NewUpgrader(func(c *Upgrader) {
			c.EventHandler = handler
		})
		server, client := testNewPeer(upgrader)
		client.NetConn().Close()
		server.WriteAsync(OpcodeText, internal.AlphabetNumeric.Generate(8))
		wg.Wait()
	})
}

// 测试异步读
func TestReadAsync(t *testing.T) {
	var handler = new(webSocketMocker)
	var upgrader = NewUpgrader(func(c *Upgrader) {
		c.EventHandler = handler
		c.CompressEnabled = true
		c.CompressionThreshold = 512
		c.AsyncReadEnabled = true
	})

	var mu = &sync.Mutex{}
	var listA []string
	var listB []string
	const count = 1000
	var wg = &sync.WaitGroup{}
	wg.Add(count)

	handler.onMessage = func(socket *Conn, message *Message) {
		mu.Lock()
		listB = append(listB, message.Data.String())
		mu.Unlock()
		wg.Done()
	}

	server, client := testNewPeer(upgrader)

	go func() {
		for i := 0; i < count; i++ {
			var n = internal.AlphabetNumeric.Intn(1024)
			var message = internal.AlphabetNumeric.Generate(n)
			listA = append(listA, string(message))
			testClientWrite(client, true, OpcodeText, message)
		}
	}()

	go server.Listen()

	wg.Wait()
	assert.ElementsMatch(t, listA, listB)
}

func TestTaskQueue(t *testing.T) {
	var as = assert.New(t)
	var mu = &sync.Mutex{}
	var listA []int
	var listB []int

	var count = 1000
	var wg = &sync.WaitGroup{}
	wg.Add(count)
	var q = newWorkerQueue(8)
	for i := 0; i < count; i++ {
		listA = append(listA, i)
		q.AddJob(asyncJob{
			Args: i,
			Do: func(args interface{}) error {
				defer wg.Done()
				var latency = time.Duration(internal.AlphabetNumeric.Intn(100)) * time.Microsecond
				time.Sleep(latency)
				mu.Lock()
				listB = append(listB, args.(int))
				mu.Unlock()
				return nil
			},
		})
	}
	wg.Wait()
	as.ElementsMatch(listA, listB)
}

func TestMessageQueue_Push(t *testing.T) {
	var as = assert.New(t)
	mq := newMessageQueue(1)
	err1 := mq.Push(OpcodeText, nil)
	err2 := mq.Push(OpcodeText, nil)
	as.Nil(err1)
	as.Error(err2)
	as.Equal(len(mq.data), 1)
}