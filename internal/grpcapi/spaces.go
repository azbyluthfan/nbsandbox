// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

func toSpace(sp spaces.Space, withUsage bool) *pb.Space {
	out := &pb.Space{Id: sp.ID, Name: sp.Name, CreatedAt: timestamppb.New(sp.CreatedAt), UpdatedAt: timestamppb.New(sp.UpdatedAt)}
	if withUsage {
		out.Usage = &pb.Usage{BytesUsed: sp.Usage.BytesUsed, QuotaBytes: sp.Usage.QuotaBytes, FileCount: sp.Usage.FileCount,
			RunCount: int32(sp.Usage.RunCount)} //nolint:gosec // a count of runs
	}
	return out
}

func toFile(f spaces.File) *pb.File {
	return &pb.File{Path: f.Path, Type: f.Type, Size: f.Size, Modified: timestamppb.New(f.Modified), Version: f.Version}
}

func toFiles(fs []spaces.File) []*pb.File {
	out := make([]*pb.File, 0, len(fs))
	for _, f := range fs {
		out = append(out, toFile(f))
	}
	return out
}

func page(cursor string, limit int32) (string, int, error) {
	if limit == 0 {
		return cursor, api.DefaultPageSize, nil
	}
	if limit < 1 || limit > api.MaxPageSize {
		return "", 0, invalid(fmt.Sprintf("limit must be between 1 and %d", api.MaxPageSize))
	}
	return cursor, int(limit), nil
}

func (s *server) CreateSpace(ctx context.Context, req *connect.Request[pb.CreateSpaceRequest]) (*connect.Response[pb.Space], error) {
	sp, err := s.d.Spaces.Create(ctx, owner(ctx), req.Msg.Name)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toSpace(sp, true)), nil
}

func (s *server) ListSpaces(ctx context.Context, req *connect.Request[pb.ListSpacesRequest]) (*connect.Response[pb.ListSpacesResponse], error) {
	cursor, limit, err := page(req.Msg.Cursor, req.Msg.Limit)
	if err != nil {
		return nil, err
	}
	list, next, err := s.d.Spaces.List(ctx, owner(ctx), cursor, limit)
	if err != nil {
		return nil, s.toErr(err)
	}
	out := &pb.ListSpacesResponse{NextCursor: next}
	for _, sp := range list {
		out.Items = append(out.Items, toSpace(sp, false))
	}
	return connect.NewResponse(out), nil
}

func (s *server) GetSpace(ctx context.Context, req *connect.Request[pb.GetSpaceRequest]) (*connect.Response[pb.Space], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	sp, err := s.d.Spaces.Get(ctx, owner(ctx), req.Msg.SpaceId)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toSpace(sp, true)), nil
}

func (s *server) UpdateSpace(ctx context.Context, req *connect.Request[pb.UpdateSpaceRequest]) (*connect.Response[pb.Space], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	sp, err := s.d.Spaces.Rename(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Name)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toSpace(sp, true)), nil
}

func (s *server) DeleteSpace(ctx context.Context, req *connect.Request[pb.DeleteSpaceRequest]) (*connect.Response[pb.DeleteSpaceResponse], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if err := s.d.Spaces.Delete(ctx, owner(ctx), req.Msg.SpaceId); err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.DeleteSpaceResponse{}), nil
}

// dataReader reads the data messages of a client stream until a message of
// another kind (kept for the caller) or the end.
type dataReader[M any] struct {
	recv    func() (*M, error) // io.EOF at the end
	data    func(*M) ([]byte, bool)
	pending *M
	buf     []byte
	done    bool
}

func (r *dataReader[M]) next() (*M, error) {
	if r.pending != nil {
		m := r.pending
		r.pending = nil
		return m, nil
	}
	return r.recv()
}

func (r *dataReader[M]) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		m, err := r.next()
		if errors.Is(err, io.EOF) {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		b, ok := r.data(m)
		if !ok {
			r.pending, r.done = m, true
			return 0, io.EOF
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func receiver[M any](st *connect.ClientStream[M]) func() (*M, error) {
	return func() (*M, error) {
		if st.Receive() {
			return st.Msg(), nil
		}
		if err := st.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
}

func (s *server) UploadFiles(ctx context.Context, st *connect.ClientStream[pb.UploadFilesRequest]) (*connect.Response[pb.UploadFilesResponse], error) {
	recv := receiver(st)
	first, err := recv()
	if err != nil || first.GetTarget() == nil {
		return nil, invalid("the first message names the space (target)")
	}
	t := first.GetTarget()
	if err := validIDs(t.SpaceId); err != nil {
		return nil, err
	}
	r := &dataReader[pb.UploadFilesRequest]{recv: recv, data: func(m *pb.UploadFilesRequest) ([]byte, bool) {
		d, ok := m.Part.(*pb.UploadFilesRequest_Data)
		if !ok {
			return nil, false
		}
		return d.Data, true
	}}
	next := func() (*spaces.UploadPart, error) {
		m, err := r.next()
		if err != nil {
			return nil, err // io.EOF: no more files
		}
		name := m.GetFileName()
		if name == "" {
			return nil, &spaces.InvalidError{Msg: "each file starts with a message naming it (file_name), then its data"}
		}
		r.done = false
		return &spaces.UploadPart{Name: name, Body: r}, nil
	}
	files, err := s.d.Spaces.Upload(ctx, owner(ctx), t.SpaceId, t.Folder, next)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.UploadFilesResponse{Files: toFiles(files)}), nil
}

func (s *server) ListFiles(ctx context.Context, req *connect.Request[pb.ListFilesRequest]) (*connect.Response[pb.ListFilesResponse], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	files, err := s.d.Spaces.ListFiles(ctx, owner(ctx), req.Msg.SpaceId)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.ListFilesResponse{Items: toFiles(files)}), nil
}

// sendFile streams f: its description, then its contents.
func sendFile(st *connect.ServerStream[pb.FileChunk], f *os.File, desc *pb.File) error {
	defer func() { _ = f.Close() }()
	if err := st.Send(&pb.FileChunk{File: desc}); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := st.Send(&pb.FileChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *server) GetFile(ctx context.Context, req *connect.Request[pb.GetFileRequest], st *connect.ServerStream[pb.FileChunk]) error {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return err
	}
	f, info, err := s.d.Spaces.OpenFile(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Path)
	if err != nil {
		return s.toErr(err)
	}
	return sendFile(st, f, toFile(info))
}

func (s *server) PutFile(ctx context.Context, st *connect.ClientStream[pb.PutFileRequest]) (*connect.Response[pb.PutFileResponse], error) {
	recv := receiver(st)
	first, err := recv()
	if err != nil || first.GetTarget() == nil {
		return nil, invalid("the first message says where (target)")
	}
	t := first.GetTarget()
	if err := validIDs(t.SpaceId); err != nil {
		return nil, err
	}
	if t.IfMatch == "*" {
		return nil, invalid("if_match must name a version, not *")
	}
	body := &dataReader[pb.PutFileRequest]{recv: recv, data: func(m *pb.PutFileRequest) ([]byte, bool) {
		d, ok := m.Part.(*pb.PutFileRequest_Data)
		if !ok {
			return nil, false
		}
		return d.Data, true
	}}
	_, statErr := s.d.Spaces.Stat(ctx, owner(ctx), t.SpaceId, t.Path)
	f, err := s.d.Spaces.WriteFile(ctx, owner(ctx), t.SpaceId, spaces.WriteRequest{Path: t.Path, Body: body,
		IfMatch: t.IfMatch, IfNoneMatch: t.IfNoneMatch})
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.PutFileResponse{File: toFile(f), Created: statErr != nil}), nil
}

func (s *server) MoveFile(ctx context.Context, req *connect.Request[pb.MoveFileRequest]) (*connect.Response[pb.File], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if req.Msg.NewPath == "" {
		return nil, invalid("new_path is required")
	}
	f, err := s.d.Spaces.Move(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Path, req.Msg.NewPath, req.Msg.IfMatch)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toFile(f)), nil
}

func (s *server) DeleteFile(ctx context.Context, req *connect.Request[pb.DeleteFileRequest]) (*connect.Response[pb.DeleteFileResponse], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if err := s.d.Spaces.DeletePath(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Path, req.Msg.Recursive); err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.DeleteFileResponse{}), nil
}

func (s *server) CreateFolder(ctx context.Context, req *connect.Request[pb.CreateFolderRequest]) (*connect.Response[pb.File], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	f, err := s.d.Spaces.CreateFolder(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Path)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toFile(f)), nil
}
