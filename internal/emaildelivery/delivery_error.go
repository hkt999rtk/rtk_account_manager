package emaildelivery

import "errors"

type DeliveryError struct {
	Err       error
	Transient bool
}

func (e *DeliveryError) Error() string { return e.Err.Error() }
func (e *DeliveryError) Unwrap() error { return e.Err }

func IsTransient(err error) bool {
	var deliveryErr *DeliveryError
	return errors.As(err, &deliveryErr) && deliveryErr.Transient
}
