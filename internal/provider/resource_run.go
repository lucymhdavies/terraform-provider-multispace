package provider

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tfe "github.com/hashicorp/go-tfe"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceRun() *schema.Resource {
	return &schema.Resource{
		Description: "Workspace run (create/destroy)",

		CreateContext: resourceRunCreate,
		ReadContext:   resourceRunRead,
		UpdateContext: resourceRunUpdate,
		DeleteContext: resourceRunDelete,

		Schema: map[string]*schema.Schema{
			"organization": {
				Description: runDescriptions["organization"],
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
			},

			"workspace": {
				Description: runDescriptions["workspace"],
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
			},

			"manual_confirm": {
				Description: runDescriptions["manual_confirm"],
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
			},

			"retry": {
				Description: runDescriptions["retry"],
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
			},

			"retry_attempts": {
				Description: runDescriptions["retry_attempts"],
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     3,
			},

			"retry_backoff_min": {
				Description: runDescriptions["retry_backoff_min"],
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     1,
			},

			"retry_backoff_max": {
				Description: runDescriptions["retry_backoff_max"],
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     30,
			},

			"do_apply": {
				Description: runDescriptions["do_apply"],
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
			},

			"do_destroy": {
				Description: runDescriptions["do_destroy"],
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
			},

			"wait_for_apply": {
				Description: runDescriptions["wait_for_apply"],
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
			},

			"wait_for_destroy": {
				Description: runDescriptions["wait_for_destroy"],
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
			},
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(15 * time.Minute),
			Delete: schema.DefaultTimeout(15 * time.Minute),
		},
	}
}

func resourceRunCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {

	// If we are just using multispace_run to ensure destroys are triggered
	doApply := d.Get("do_apply").(bool)
	if !doApply {
		d.SetId("no-op")
		return nil
	}
	// Otherwise, proceed as normal

	return doRun(ctx, d, meta, false)
}

func resourceRunRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {

	// If we are just using multispace_run to ensure destroys are triggered
	doApply := d.Get("do_apply").(bool)
	if !doApply {
		d.SetId("no-op")
		return nil
	}
	// Otherwise, proceed as normal

	client := meta.(*tfe.Client)
	id := d.Id()

	// Get our run. If it doesn't exist, then we assume that we were never
	// created. And if it exists in any form, we assume we're still in that
	// state.
	_, err := client.Runs.Read(ctx, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			d.SetId("")
			return nil
		}

		return diag.FromErr(err)
	}

	return nil
}

func resourceRunUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	// Update we do nothing since we should have created during apply.
	return nil
}

func resourceRunDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	// If we are just using multispace_run to ensure destroys are triggered
	doDestroy := d.Get("do_destroy").(bool)
	if !doDestroy {
		return nil
	}
	// Otherwise, proceed as normal

	return doRun(ctx, d, meta, true)
}

func doRun(
	ctx context.Context,
	d *schema.ResourceData,
	meta interface{},
	destroy bool,
) diag.Diagnostics {
	// We only set the ID on create
	setId := func(v string) {
		if !destroy {
			d.SetId(v)
		}
	}

	// Get our retry information
	retry := d.Get("retry").(bool)
	retryMaxAttempts := d.Get("retry_attempts").(int)
	retryBOMin := d.Get("retry_backoff_min").(int)
	retryBOMax := d.Get("retry_backoff_max").(int)
	retryCurAttempts := 0

RETRY:
	retryCurAttempts++
	if retryCurAttempts > 1 {
		// If we're retrying, then perform the backoff.
		select {
		case <-ctx.Done():
			return diag.FromErr(ctx.Err())
		case <-time.After(backoff(
			float64(retryBOMin), float64(retryBOMax), retryCurAttempts)):
		}
	}
	if retryCurAttempts > retryMaxAttempts {
		return diag.Errorf(
			"Maximum retry attempts %d reached. Please see the web UI "+
				"to see any errors during plan or apply.",
			retryMaxAttempts,
		)
	}

	client := meta.(*tfe.Client)
	org := d.Get("organization").(string)
	workspace := d.Get("workspace").(string)

	// Get our workspace because we need it to queue a plan
	ws, err := client.Workspaces.Read(ctx, org, workspace)
	if err != nil {
		return diag.FromErr(err)
	}

	// Get our wait information
	waitForApply := d.Get("wait_for_apply").(bool)
	if destroy {
		waitForApply = d.Get("wait_for_destroy").(bool)
	}

	runOptions := tfe.RunCreateOptions{
		Message: tfe.String(fmt.Sprintf(
			"terraform-provider-multispace on %s",
			time.Now().Format("Mon Jan 2 15:04:05 MST 2006"),
		)),
		Workspace: ws,
		IsDestroy: tfe.Bool(destroy),
	}
	if waitForApply {
		// Do auto-apply because we handle all that.
		runOptions.AutoApply = tfe.Bool(false)
		// Unless this is a fire-and-forget Apply, in which case, do the
		// workspace default behaviour
	}

	// Create a run
	run, err := client.Runs.Create(ctx, runOptions)

	// I THINK if err != nil implies run == nil but we can never be too sure.
	// If we have a non-nil run, we want to save the ID so we don't dangle
	// resources.
	if run != nil {
		// The ID we use is the run we queue. We can use this to look this
		// run up again in the case of a partial failure.
		setId(run.ID)
		log.Printf("[INFO] run created: %s", run.ID)
	}

	if err != nil {
		return diag.FromErr(err)
	}

	// If we reach this point and run is nil, there is a bug in the TFE
	// API client. But its non-sensical and we can't continue so we must exit.
	if run == nil {
		log.Printf("[ERROR] TFE client returned err nil but also run nil")
		return diag.Errorf(
			"INTERNAL ERROR: run create API did not error, but also did not " +
				"return a run ID.")
	}

	if waitForApply {
		// Wait for the plan to complete.
		run, diags := waitForRun(ctx, client, org, run, ws, true, []tfe.RunStatus{
			tfe.RunPlanned,
			tfe.RunPlannedAndFinished,
			tfe.RunErrored,
			tfe.RunCostEstimated,
			tfe.RunPolicyChecked,
			tfe.RunPolicySoftFailed,
			tfe.RunPolicyOverride,
		}, []tfe.RunStatus{
			tfe.RunPending,
			tfe.RunPlanQueued,
			tfe.RunPlanning,
			tfe.RunCostEstimating,
			tfe.RunPolicyChecking,
		})
		if diags != nil {
			return diags
		}

		// If the run errored, we should have exited already but lets just exit now.
		if run.Status == tfe.RunErrored || run.Status == tfe.RunPolicySoftFailed {
			// Clear the ID, we didn't create anything.
			setId("")

			if retry {
				// Retry
				goto RETRY
			}

			return diag.Errorf(
				"Run %q errored during plan. Please open the web UI to view the error",
				run.ID,
			)
		}

		// If the plan has no changes, then we're done.
		if !run.HasChanges || run.Status == tfe.RunPlannedAndFinished {
			log.Printf("[INFO] plan finished, no changes")
			return nil
		}

		// If a policy soft-fails, we need human approval before we continue
		if run.Status == tfe.RunPolicyOverride {
			log.Printf("[INFO] policy check soft-failed, waiting for manual override. %q", run.ID)
			run, diags = waitForRun(ctx, client, org, run, ws, true, []tfe.RunStatus{
				tfe.RunConfirmed,
				tfe.RunApplyQueued,
				tfe.RunApplying,
			}, []tfe.RunStatus{run.Status})
			if diags != nil {
				return diags
			}
		}

		// If we're doing a manual confirmation, then we wait for the human to confirm.
		if !destroy && d.Get("manual_confirm").(bool) {
			log.Printf("[INFO] plan complete, waiting for manual confirm. %q", run.ID)
			run, diags = waitForRun(ctx, client, org, run, ws, true, []tfe.RunStatus{
				tfe.RunConfirmed,
				tfe.RunApplyQueued,
				tfe.RunApplying,
			}, []tfe.RunStatus{run.Status})
			if diags != nil {
				return diags
			}
		} else {
			// Apply the plan.
			log.Printf("[INFO] plan complete, confirming apply. %q", run.ID)
			if err := client.Runs.Apply(ctx, run.ID, tfe.RunApplyOptions{
				Comment: tfe.String(fmt.Sprintf(
					"terraform-provider-multispace on %s",
					time.Now().Format("Mon Jan 2 15:04:05 MST 2006"),
				)),
			}); err != nil {
				return diag.FromErr(err)
			}
		}

		// Wait now for the apply to complete
		run, diags = waitForRun(ctx, client, org, run, ws, false, []tfe.RunStatus{
			tfe.RunApplied,
			tfe.RunErrored,
		}, []tfe.RunStatus{
			tfe.RunConfirmed,
			tfe.RunApplyQueued,
			tfe.RunApplying,
		})
		if diags != nil {
			return diags
		}

		// If the run errored, we should have exited already but lets just exit now.
		if run.Status == tfe.RunErrored {
			// Clear the ID, we didn't create anything.
			setId("")

			if retry {
				// Retry
				goto RETRY
			}

			return diag.Errorf(
				"Run %q errored during apply. Please open the web UI to view the error",
				run.ID,
			)
		}

		// If this is not applied, we're in some unexpected state.
		if run.Status != tfe.RunApplied {
			setId("")

			return diag.Errorf(
				"Run %q entered unexpected state %q, expected applied",
				run.ID, run.Status,
			)
		}
	}

	return nil
}

var runDescriptions = map[string]string{
	"organization": "The name of the Terraform Cloud organization that owns the workspace.",
	"workspace":    "The name of the Terraform Cloud workspace to execute.",
	"manual_confirm": "If true, a human will have to manually confirm a plan " +
		"to start the apply. This applies to the creation only. Destroy never " +
		"requires manual confirmation. This requires a human to carefully watch the execution " +
		"of this Terraform run and hit the 'confirm' button. Be aware of resource " +
		"timeouts during the Terraform run.",
	"retry": "Whether or not to retry on plan or apply errors.",
	"retry_attempts": "The number of retry attempts made for any errors during " +
		"plan or apply. This applies to both creation and destruction.",
	"retry_backoff_min": "The minimum seconds to wait between retry attempts.",
	"retry_backoff_max": "The maximum seconds to wait between retry attempts. Retries " +
		"are done using an exponential backoff, so this can be used to limit " +
		"the maximum time between retries.",
	"wait_for_apply": "Whether or not to wait for the Apply to succeed before " +
		"considering the resource created",
	"wait_for_destroy": "Whether or not to wait for the Destroy to succeed before " +
		"considering the resource destroyed",
	"do_apply": "Whether or not to trigger an Apply run when creating the resource. " +
		"For example, if you wish to ensure that Terraform triggers a destroy on all your " +
		"workspaces, before deleting them, but you wish to kick off your first apply manually",
	"do_destroy": "Whether or not to trigger a Destroy run when destroying the resource. " +
		"For example, if destroying some workspaces is unnecessary",
}
