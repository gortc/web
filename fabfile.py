from fabric.api import local, put, run, cd, sudo

def status():
   run("systemctl status web")

def restart():
   sudo("systemctl restart web")

def deploy():
    local('tar -czf cydev_web.tgz web static/')
    put("cydev_web.tgz", "~/cydev.ru")
    with cd("~/cydev.ru"):
        run("tar -xvf cydev_web.tgz")
    restart()
    status()
