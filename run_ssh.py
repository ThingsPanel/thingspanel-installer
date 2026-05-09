import paramiko
import sys

def run_cmd(cmd):
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    try:
        client.connect('47.115.213.71', username='root', password='TP-2024/dev.NeW-0117', timeout=10)
        stdin, stdout, stderr = client.exec_command(cmd)
        print('STDOUT:')
        print(stdout.read().decode('utf-8'))
        print('STDERR:')
        print(stderr.read().decode('utf-8'))
    finally:
        client.close()

if __name__ == '__main__':
    run_cmd(sys.argv[1])
