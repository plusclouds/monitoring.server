package api

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/plugins/push"
)

// ---- MQTT ingest (F08, F12) ----

func toAPIMqttCredential(c inventory.MQTTCredential) gen.MqttCredential {
	out := gen.MqttCredential{
		Id: c.ID, Name: c.Name, Username: c.Username, Kind: gen.MqttCredentialKind(c.Kind), DeviceKey: c.DeviceKey,
		Profile: gen.MqttCredentialProfile(c.Profile), AllowPlain: c.AllowPlain, AutoRegister: c.AutoRegister,
		Enabled: c.Enabled, CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(), LastUsedAt: c.LastUsedAt,
	}
	if c.Password != "" {
		out.Password = &c.Password
	}
	return out
}

func (s *Server) ListMqttCredentials(ctx context.Context, _ gen.ListMqttCredentialsRequestObject) (gen.ListMqttCredentialsResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var list []inventory.MQTTCredential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		list, err = inventory.ListMQTTCredentials(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ListMqttCredentials200JSONResponse{Items: make([]gen.MqttCredential, len(list))}
	for i, c := range list {
		out.Items[i] = toAPIMqttCredential(c)
	}
	return out, nil
}

func (s *Server) CreateMqttCredential(ctx context.Context, req gen.CreateMqttCredentialRequestObject) (gen.CreateMqttCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := inventory.MQTTCredentialInput{Name: b.Name, Kind: string(b.Kind), DeviceKey: b.DeviceKey,
		AutoRegister: b.AutoRegister, Enabled: b.Enabled}
	deref(&in.Username, b.Username)
	deref(&in.Password, b.Password)
	deref(&in.AllowPlain, b.AllowPlain)
	if b.Profile != nil {
		in.Profile = string(*b.Profile)
	}
	var c inventory.MQTTCredential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = inventory.CreateMQTTCredential(ctx, tx, p.Actor, t.ID, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateMqttCredential201JSONResponse(toAPIMqttCredential(c)), nil
}

func (s *Server) GetMqttCredential(ctx context.Context, req gen.GetMqttCredentialRequestObject) (gen.GetMqttCredentialResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var c inventory.MQTTCredential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = inventory.GetMQTTCredential(ctx, tx, req.CredentialId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetMqttCredential200JSONResponse(toAPIMqttCredential(c)), nil
}

func (s *Server) PatchMqttCredential(ctx context.Context, req gen.PatchMqttCredentialRequestObject) (gen.PatchMqttCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var c inventory.MQTTCredential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := inventory.GetMQTTCredential(ctx, tx, req.CredentialId)
		if err != nil {
			return err
		}
		name, plain, auto, enabled := cur.Name, cur.AllowPlain, cur.AutoRegister, cur.Enabled
		deref(&name, req.Body.Name)
		deref(&plain, req.Body.AllowPlain)
		deref(&auto, req.Body.AutoRegister)
		deref(&enabled, req.Body.Enabled)
		c, err = inventory.UpdateMQTTCredential(ctx, tx, p.Actor, req.CredentialId, name, plain, auto, enabled)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PatchMqttCredential200JSONResponse(toAPIMqttCredential(c)), nil
}

func (s *Server) DeleteMqttCredential(ctx context.Context, req gen.DeleteMqttCredentialRequestObject) (gen.DeleteMqttCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	if err := s.tx(ctx, t, func(tx pgx.Tx) error {
		return inventory.DeleteMQTTCredential(ctx, tx, p.Actor, req.CredentialId)
	}); err != nil {
		return nil, err
	}
	return gen.DeleteMqttCredential204Response{}, nil
}

func (s *Server) RotateMqttCredential(ctx context.Context, req gen.RotateMqttCredentialRequestObject) (gen.RotateMqttCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var password string
	if req.Body != nil {
		deref(&password, req.Body.Password)
	}
	var c inventory.MQTTCredential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = inventory.RotateMQTTCredential(ctx, tx, p.Actor, req.CredentialId, password)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.RotateMqttCredential200JSONResponse(toAPIMqttCredential(c)), nil
}

func (s *Server) ListIngestProfiles(ctx context.Context, _ gen.ListIngestProfilesRequestObject) (gen.ListIngestProfilesResponseObject, error) {
	if _, _, err := tenantScope(ctx, roleReadOnly, false); err != nil {
		return nil, err
	}
	var out gen.ListIngestProfiles200JSONResponse
	for _, p := range push.Profiles {
		out.Items = append(out.Items, struct {
			Description string `json:"description"`
			Name        string `json:"name"`
		}{Description: p.Description, Name: p.Name})
	}
	return out, nil
}

func (s *Server) ListUnregistered(ctx context.Context, _ gen.ListUnregisteredRequestObject) (gen.ListUnregisteredResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var list []inventory.Unregistered
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		list, err = inventory.ListUnregistered(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := via[gen.ListUnregistered200JSONResponse](map[string]any{"items": nonNilSlice(list)})
	return out, err
}

func toAPIDeviceMqtt(m inventory.MQTTDevice) (gen.DeviceMqtt, error) {
	return via[gen.DeviceMqtt](map[string]any{"device_id": m.DeviceID, "device_key": m.DeviceKey,
		"data_check_id": m.DataCheckID, "connection_check_id": m.ConnectionCheckID, "created_at": m.CreatedAt.UTC(),
		"session": m.Session})
}

func (s *Server) GetDeviceMqtt(ctx context.Context, req gen.GetDeviceMqttRequestObject) (gen.GetDeviceMqttResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var m inventory.MQTTDevice
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		m, err = inventory.GetMQTTDevice(ctx, tx, req.DeviceId)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toAPIDeviceMqtt(m)
	return gen.GetDeviceMqtt200JSONResponse(out), err
}

func (s *Server) BindDeviceMqtt(ctx context.Context, req gen.BindDeviceMqttRequestObject) (gen.BindDeviceMqttResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	profile := ""
	if req.Body.Profile != nil {
		profile = string(*req.Body.Profile)
	}
	var m inventory.MQTTDevice
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		m, err = inventory.BindMQTT(ctx, tx, p.Actor, req.DeviceId, req.Body.DeviceKey, profile, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toAPIDeviceMqtt(m)
	return gen.BindDeviceMqtt200JSONResponse(out), err
}

func (s *Server) UnbindDeviceMqtt(ctx context.Context, req gen.UnbindDeviceMqttRequestObject) (gen.UnbindDeviceMqttResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	if err := s.tx(ctx, t, func(tx pgx.Tx) error { return inventory.UnbindMQTT(ctx, tx, p.Actor, req.DeviceId) }); err != nil {
		return nil, err
	}
	return gen.UnbindDeviceMqtt204Response{}, nil
}
