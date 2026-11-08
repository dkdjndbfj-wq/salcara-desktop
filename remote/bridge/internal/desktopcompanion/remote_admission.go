package desktopcompanion

import "context"

type remoteAdmissionKey struct{}

// WithRemoteAdmissionGuard carries the caller's short-lived phone/station
// authority without changing the persistent local desktop activation lease.
func WithRemoteAdmissionGuard(ctx context.Context, check func() error) context.Context {
	return context.WithValue(ctx, remoteAdmissionKey{}, check)
}

func CheckRemoteAdmission(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(remoteAdmissionKey{}).(func() error); ok && check != nil {
		return check()
	}
	return nil
}
