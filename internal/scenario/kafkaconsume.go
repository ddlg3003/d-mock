package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/ddlg3003/d-mock/internal/tmpl"
	"github.com/segmentio/kafka-go"
)

// kafkaConsume reads what was published to a topic since the scenario started (minus a 1s
// margin for clock differences), without joining a consumer group, so it never takes
// messages away from the service under test and can be retried as often as needed.
//
//	consume: {topic, key: K, match: {json.path: value, ...}, since: 30s}
//
// key and match filter the messages (match compares JSON paths as text); since reads from
// that long before the scenario started instead. Result: count, messages[] with key, value,
// json (when the value is JSON), headers, partition, offset, time — oldest first.
func (ru *run) kafkaConsume(ctx context.Context, brokers []string, c map[string]any) (map[string]any, error) {
	topic := str(c, "topic")
	if topic == "" {
		return nil, errors.New("kafka consume: topic is required")
	}
	from := ru.start.Add(-time.Second)
	if s := str(c, "since"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("kafka consume: since: %w", err)
		}
		from = ru.start.Add(-d)
	}
	match, _ := c["match"].(map[string]any)
	key, hasKey := c["key"]
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	conn, err := kafka.DialContext(cctx, "tcp", brokers[0])
	if err != nil {
		return nil, err
	}
	parts, err := conn.ReadPartitions(topic)
	conn.Close()
	if err != nil {
		return nil, fmt.Errorf("kafka consume %s: %w", topic, err)
	}
	var msgs []map[string]any
	for _, p := range parts {
		leader := net.JoinHostPort(p.Leader.Host, strconv.Itoa(p.Leader.Port))
		got, err := readPartitionSince(cctx, leader, topic, p.ID, from)
		if err != nil {
			return nil, fmt.Errorf("kafka consume %s[%d]: %w", topic, p.ID, err)
		}
		for _, m := range got {
			if hasKey && string(m.Key) != tmpl.Stringify(key) {
				continue
			}
			entry := map[string]any{"key": string(m.Key), "value": string(m.Value), "partition": m.Partition, "offset": m.Offset, "time": m.Time.UTC().Format(time.RFC3339Nano)}
			var j any
			if json.Unmarshal(m.Value, &j) == nil {
				entry["json"] = j
			}
			hs := map[string]any{}
			for _, h := range m.Headers {
				hs[h.Key] = string(h.Value)
			}
			entry["headers"] = hs
			if !matches(entry, match) {
				continue
			}
			msgs = append(msgs, entry)
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i]["time"].(string) < msgs[j]["time"].(string) })
	list := make([]any, len(msgs))
	for i, m := range msgs {
		list[i] = m
	}
	return map[string]any{"topic": topic, "count": len(list), "messages": list}, nil
}

// matches reports whether every path in want has that value (compared as text) in entry.
func matches(entry map[string]any, want map[string]any) bool {
	for path, v := range want {
		got, ok := tmpl.Lookup(entry, path)
		if !ok || tmpl.Stringify(got) != tmpl.Stringify(v) {
			return false
		}
	}
	return true
}

// readPartitionSince returns the messages written to one partition at or after `from`, up to
// the end of the partition as it is now.
func readPartitionSince(ctx context.Context, leader, topic string, partition int, from time.Time) ([]kafka.Message, error) {
	conn, err := kafka.DialLeader(ctx, "tcp", leader, topic, partition)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	first, err := conn.ReadOffset(from)
	if err != nil {
		return nil, err
	}
	last, err := conn.ReadLastOffset()
	if err != nil {
		return nil, err
	}
	if first < 0 || first >= last {
		return nil, nil
	}
	if _, err := conn.Seek(first, kafka.SeekAbsolute); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	var out []kafka.Message
	next := first
	for next < last { // a batch can end before the partition does: fetch until `last`
		batch := conn.ReadBatch(1, 10<<20)
		progressed := false
		for {
			m, err := batch.ReadMessage()
			if err != nil {
				break
			}
			progressed = true
			next = m.Offset + 1
			out = append(out, m)
			if next >= last {
				break
			}
		}
		if err := batch.Close(); err != nil && !progressed {
			return out, err
		}
		if !progressed {
			break
		}
	}
	return out, nil
}
