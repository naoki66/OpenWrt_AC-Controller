package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

type MeshState struct {
	APMAC      string           `json:"ap_mac"`
	UpdatedAt  int64            `json:"updated_at"`
	Interfaces []proto.MeshStat `json:"interfaces"`
}

func (s *Store) RecordMesh(ctx context.Context, mac string, stats []proto.MeshStat) error {
	if stats == nil {
		// 回退到不支持 Mesh 上报的旧版 agent 时，清除之前的链路记录。
		_, err := s.db.ExecContext(ctx, "DELETE FROM mesh_state WHERE ap_mac=?", mac)
		return err
	}
	body, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mesh_state(ap_mac,report,updated_at) VALUES(?,?,?)
		ON CONFLICT(ap_mac) DO UPDATE SET report=excluded.report,updated_at=excluded.updated_at`, mac, string(body), time.Now().Unix())
	return err
}

func (s *Store) LatestMesh(ctx context.Context) ([]MeshState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.ap_mac,m.report,m.updated_at FROM mesh_state m JOIN devices d ON d.mac=m.ap_mac ORDER BY m.ap_mac`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := make([]MeshState, 0)
	for rows.Next() {
		var st MeshState
		var raw string
		if err := rows.Scan(&st.APMAC, &raw, &st.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &st.Interfaces); err != nil {
			return nil, err
		}
		list = append(list, st)
	}
	return list, rows.Err()
}
