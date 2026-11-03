# Set up a fixed outbound IP for the crawler (Google Cloud)

Payers that block automated access (BCBS Kansas, for example) can only allowlist us if every
request comes from one known address. GitHub-hosted runners change IP on every run, so the
crawler runs on a small Google Cloud VM with a reserved static IP, registered as a GitHub
self-hosted runner.

Rough cost (us-east4, on-demand, 2026 list prices): e2-small about $13/month, static IPv4 in use
about $3.65/month, 50 GB balanced disk about $5/month. About $20/month in total. Downloads into
the VM are free; only traffic leaving Google is billed, and the crawler sends very little.

Run every command in [Cloud Shell](https://shell.cloud.google.com) (top right of the console,
the `>_` icon), signed in as the project owner.

## 1. Project and billing

```bash
gcloud projects create tic-crawler-prod --name="TiC crawler"
gcloud config set project tic-crawler-prod
gcloud billing accounts list                       # copy the ACCOUNT_ID
gcloud billing projects link tic-crawler-prod --billing-account=ACCOUNT_ID
gcloud services enable compute.googleapis.com iap.googleapis.com billingbudgets.googleapis.com
```

Use an existing project instead if the Drive service account already lives in one; skip
`projects create` and set that project.

Set a budget alert so a mistake cannot run up a bill:

```bash
gcloud billing budgets create --billing-account=ACCOUNT_ID \
  --display-name="tic-crawler" --budget-amount=50USD \
  --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0
```

## 2. Reserve the static IP

```bash
gcloud compute addresses create tic-crawler-egress --region=us-east4
gcloud compute addresses describe tic-crawler-egress --region=us-east4 --format='value(address)'
```

That address is what payers allowlist. Write it down.

## 3. Service account for the VM (no permissions)

```bash
gcloud iam service-accounts create tic-crawler-vm --display-name="tic-crawler VM"
```

It gets no roles. Google credentials for Drive and GCS stay in GitHub secrets, as today.

## 4. Create the VM

```bash
gcloud compute instances create tic-crawler-1 \
  --zone=us-east4-a --machine-type=e2-small \
  --image-family=ubuntu-2404-lts-amd64 --image-project=ubuntu-os-cloud \
  --boot-disk-size=50GB --boot-disk-type=pd-balanced \
  --address=tic-crawler-egress \
  --service-account=tic-crawler-vm@tic-crawler-prod.iam.gserviceaccount.com --scopes=cloud-platform \
  --shielded-secure-boot --shielded-vtpm --shielded-integrity-monitoring \
  --tags=tic-crawler
```

Ubuntu 24.04 ships Python 3.12, which the crawler requires.

## 5. Lock down inbound access

No inbound ports are needed. Allow SSH only through Google's Identity-Aware Proxy:

```bash
gcloud compute firewall-rules create allow-ssh-iap --network=default \
  --direction=INGRESS --action=allow --rules=tcp:22 \
  --source-ranges=35.235.240.0/20 --target-tags=tic-crawler
gcloud compute firewall-rules delete default-allow-ssh default-allow-rdp --quiet   # if present
```

## 6. Install dependencies

```bash
gcloud compute ssh tic-crawler-1 --zone=us-east4-a --tunnel-through-iap
# on the VM:
sudo apt-get update && sudo apt-get install -y python3.12-venv python3-pip git curl unattended-upgrades
curl -s https://ifconfig.me; echo      # must print the reserved IP from step 2
```

## 7. Register the VM as a GitHub self-hosted runner

1. In GitHub: **tic-crawler → Settings → Actions → Runners → New self-hosted runner → Linux x64**.
2. On the VM, run the download and `./config.sh` commands GitHub shows, adding
   `--labels tic-egress --unattended` to `config.sh`.
3. Install it as a service so it survives reboots:

```bash
sudo ./svc.sh install && sudo ./svc.sh start
```

The runner shows as **Idle** on the Runners page when it is ready.

Keep the repository private while a self-hosted runner is attached: on a public repository,
anyone who opens a pull request could run code on the VM.

## 8. Point the workflows at it

In `crawl.yml` and `dry-run.yml`, change `runs-on: ubuntu-latest` to
`runs-on: [self-hosted, tic-egress]` for the crawl jobs. Leave `ci.yml` on GitHub-hosted runners.
Add one step at the start of the crawl job so each run log records the egress IP:

```yaml
      - run: echo "egress ip $(curl -s https://ifconfig.me)"
```

## 9. After it works

- Send the IP to the person handling access requests; the drafted emails to blocking payers
  cite it along with the user agent `EverBetter-TiC-Crawler/1.0 (+contact email)`.
- Optional: buy a 1-year committed-use discount for the VM once the setup is stable (about 37% off).
- To stop all charges: `gcloud compute instances delete tic-crawler-1 --zone=us-east4-a` and
  `gcloud compute addresses delete tic-crawler-egress --region=us-east4`. Deleting the address
  gives up the IP, so payers would have to allowlist a new one.
