package domain

func NormalizeLimit(limit int32) int32 {
	if limit <= 0 || limit > 100 {
		return 20
	}
	return limit
}

func CanChangeOrderStatus(current, next string) bool {
	switch current {
	case "pending_confirmation":
		return next == "accepted" || next == "rejected"
	case "accepted":
		return next == "preparing"
	case "preparing":
		return next == "ready"
	case "ready":
		return next == "delivering"
	case "delivering":
		return next == "delivered"
	default:
		return false
	}
}
