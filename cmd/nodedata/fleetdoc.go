package main

// fleetAPIVersion 是 /api/fleet 的契约版本。只加字段时不动它；
// 改了已有字段的含义才加一，巡视台据此决定怎么读。
const fleetAPIVersion = 1

// fleetDoc 是巡视台每轮从本机拉走的全部内容。
type fleetDoc struct {
	V       int      `json:"v"`
	Host    string   `json:"host"`
	Version string   `json:"version"`
	At      int64    `json:"at"` // 本机 unix 秒
	Rows    []UseRow `json:"rows"`
}
