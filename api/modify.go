package api

import "log"

// Action represents one action in a bulk modify requests.
type Action struct {
	Action string `json:"action"`
	ItemID int    `json:"item_id,string"`
}

// NewArchiveAction creates an archive action.
func NewArchiveAction(itemID int) *Action {
	return &Action{
		Action: "archive",
		ItemID: itemID,
	}
}

// NewDeleteAction creates a delete action.
func NewDeleteAction(itemID int) *Action {
	return &Action{
		Action: "delete",
		ItemID: itemID,
	}
}

// ModifyResult represents the modify API's result.
type ModifyResult struct {
	// The results for each of the requested actions.
	ActionResults []bool        `json:"action_results"`
	ActionErrors  []interface{} `json:"action_errors"`
	Status        int           `json:"status"`
}

type modifyAPIOptionsWithAuth struct {
	Actions []*Action `json:"actions"`
	authInfo
}

// Modify requests bulk modification on items.
func (c *Client) Modify(actions ...*Action) (*ModifyResult, error) {
	res := &ModifyResult{}
	data := modifyAPIOptionsWithAuth{
		authInfo: c.authInfo,
		Actions:  actions,
	}
	err := PostJSON("/v3/send", data, res)
	if err != nil {
		return nil, err
	}
	for i, r := range res.ActionResults {
		if !r {
			log.Printf("Action %q on item %d failed", actions[i].Action, actions[i].ItemID)
		}
	}

	return res, nil
}
