package utils

type Listener struct {
	ID                    string            `json:"id"`
	Namespace             string            `json:"namespace"`
	Name                  string            `json:"name"`
	Target                string            `json:"target"`
	Filters               map[string]string `json:"filters"`
	PurgatorySubscription string            `json:"purgatorySubscription"`
}
