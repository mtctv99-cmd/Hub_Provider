import os
import sys
import paramiko

HOST = os.environ.get('SSH_HOST', '')
PORT = int(os.environ.get('SSH_PORT', '22'))
USER = os.environ.get('SSH_USER', '')
KEY_PATH = os.environ.get('SSH_KEY_PATH', '')

def run_ssh(cmd, timeout=300):
    client = paramiko.SSHClient()
    client.load_system_host_keys()
    client.set_missing_host_key_policy(paramiko.RejectPolicy())
    try:
        if not HOST or not USER or not KEY_PATH:
            raise RuntimeError('set SSH_HOST, SSH_USER, and SSH_KEY_PATH')
        client.connect(HOST, port=PORT, username=USER, key_filename=KEY_PATH, timeout=10)
        stdin, stdout, stderr = client.exec_command(cmd, timeout=timeout)
        out = stdout.read().decode('utf-8', errors='ignore')
        err = stderr.read().decode('utf-8', errors='ignore')
        code = stdout.channel.recv_exit_status()
        client.close()
        return code, out, err
    except Exception as e:
        return -1, '', str(e)

if __name__ == '__main__':
    if len(sys.argv) < 2:
        print("Usage: python run_remote.py <command>")
        sys.exit(1)
    command = " ".join(sys.argv[1:])
    code, out, err = run_ssh(command)
    if out:
        sys.stdout.write(out)
    if err:
        sys.stderr.write(err)
    sys.exit(code)
