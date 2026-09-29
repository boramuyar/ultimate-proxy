# Free test server on Oracle Cloud

Oracle Cloud's Always Free tier includes Ampere (ARM) VMs that don't expire. `setup.sh` turns a fresh
Ubuntu VM into a running proxy with Postgres and the fake upstream, so you can test without paying for
models.

## 1. Create the VM (Oracle console)

1. Sign up at cloud.oracle.com. A card is needed for identity checks, but Always Free resources are not charged.
2. **Compute → Instances → Create instance**
   - Image: **Canonical Ubuntu 24.04**
   - Shape: **Ampere → VM.Standard.A1.Flex**, 1–2 OCPUs, 6–12 GB memory (within the free allowance)
   - Networking: keep the default VCN and **assign a public IPv4 address**
   - SSH keys: upload your public key or download the generated one
   - **Show advanced options → Management → Paste cloud-init script**: paste the contents of `setup.sh`
3. Create. If Oracle reports "out of capacity" for A1, retry later or pick another availability domain.

## 2. Open ports 8080 and 3000

**Networking → Virtual cloud networks → your VCN → Security Lists → Default → Add ingress rule**:
source `0.0.0.0/0`, TCP, destination ports `8080,3000` (the proxy and the dashboard). (`setup.sh` opens the VM's own firewall.)

## 3. Get your credentials

The first boot takes a few minutes (it builds the image). Then:

```sh
ssh ubuntu@<public ip>
sudo tail -20 /var/log/ultimate-proxy-setup.log   # shows the URL, admin token and demo key
```

Credentials live in `/opt/ultimate-proxy/.env`. To use real models, add `OPENAI_API_KEY` there and rerun `sudo bash /opt/ultimate-proxy/deploy/oracle/setup.sh`. Rerunning also
pulls the latest `main`.

Models: `fake-gpt` (free, fake answers), and `smart` and `fast` (OpenAI).

The proxy is served over plain HTTP on port 8080. Put it behind HTTPS before sending real keys or
prompts over the internet.
