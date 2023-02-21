#!/usr/bin/env python
import io
import tarfile
from typing import Literal
import pika
import time
import json
import os
from dotenv import load_dotenv
from redis.sentinel import Sentinel
import time
import redis
import requests
import docker


class Sandbox:
    def __init__(self, image: str, dind:docker.DockerClient, command: str, workdir = "/app", cpu_period: int = 1000000, mem_limit: str = "100m", pids_limit: int = 500):
        self.image = image
        self.dind = dind
        self.container = self.dind.containers.create(
            self.image,
            command,
            working_dir=workdir,
            stdin_open=True,
            tty=False,
            detach=True,
            cpu_period=cpu_period,
            pids_limit=pids_limit,
            mem_limit=mem_limit,
            volumes={f'{workdir}/code': {'bind': workdir, 'mode': 'ro'}})
        self.container.start()
        self.stdin = self.container.attach_socket(params={'stdin': 1, 'stdout': 0, 'stderr': 0, 'stream': 1})
        self.stdin._sock.setblocking(0)
        self.stdin._writing = True

    def write(self, data):
        self.stdin.write(data.encode("utf-8"))
        self.__wait(timeout=2)

    def __wait(self, timeout: int = 2):
        try:
            self.container.wait(timeout=timeout)
        except Exception:
            raise

    def output(self):
        return self.container.logs(stdout=True, stderr=False), self.container.logs(stdout=False, stderr=True)

    @staticmethod
    def _make_archive(filename: str, data: bytes):
        tarstream = io.BytesIO()
        tar = tarfile.open(fileobj=tarstream, mode='w')
        tarinfo = tarfile.TarInfo(name=filename)
        tarinfo.size = len(data)
        tarinfo.mtime = int(time.time())
        tar.addfile(tarinfo=tarinfo, fileobj=io.BytesIO(data))
        tar.close()
        tarstream.seek(0)
        return tarstream

    def add_file(self, filename: str, data: str) -> None:
        tarstream = self._make_archive(filename, data.encode('utf-8'))
        self.dind.api.put_archive(self.container.id, WORK_DIR, tarstream)

    def status(self) -> dict[Literal["Status", "Running", "Paused", "Restarting",
                                     "OOMKilled", "Dead", "Pid", "ExitCode", "Error", "StartedAt", "FinishedAt"]]:
        self.container.reload()
        return self.container.attrs['State']

    def __del__(self):
        self.container.remove(force=True)
        self.stdin.close()


class Worker():
    def __init__(self, envrionment ,**languages):
        self.languages = languages
        self.redis = self.__init_redis(envrionment)
        self.queue = self.__init_rabbitmq(envrionment)
        self.dind = self.__dind()
        self.workdir = os.getenv("WORK_DIR").strip()
        self.__load_interpreter(self.dind, self.languages)

    def run(self, queue_name: str):
        self.queue.basic_qos(prefetch_count=1)
        self.queue.queue_declare(queue=queue_name.strip(), durable=True)
        print(' [*] Waiting for messages. To exit press CTRL+C')
        self.queue.basic_consume(queue=queue_name, on_message_callback=self.__callback)
        self.queue.start_consuming()

    def save_code(self, code: str, language: str):
        """
        Save the code to a file

        Args:
            code:
                The code to be saved
            language:
                The language of the code
        Returns:
            None
        Raises:
            None
        """
        if (language == "python"):
            with open("code/code.py", "w") as f:
                f.write(code)
        elif (language == "nodejs"):
            with open("code/code.js", "w") as f:
                f.write(code)
        else:
            print("Language not supported")

    def read_file(self, filename):
        with open(filename, "r") as f:
            return f.read()

    def __execute(self, language):
        if (language == "python"):
            sandbox = Sandbox(self.languages[language], self.dind, command="python code.py")
        elif (language == "nodejs"):
            sandbox = Sandbox(self.languages[language], self.dind, command="node code.js")
        else:
            return "", "Language not supported"
        # Pass input to stdin
        try:
            sandbox.write(self.read_file(f'{WORK_DIR}/code/input.txt'))
        except Exception as e:
            print(sandbox.status())
            return "", e.__str__()
        stdout, stderr = sandbox.output()
        del sandbox
        return stdout, stderr

    def __save_input(self, input: str):
        """
        Save the input to a file

        Args:
            input:
                The input to be saved
        Returns:
            None
        Raises:
            None
        """
        with open("code/input.txt", "w") as f:
            f.write(input)

    def __clean_up(self, langauge):
        os.remove("code/input.txt")
        if (langauge == "nodejs"):
            os.remove("code/code.js")
        elif (langauge == "python"):
            os.remove("code/code.py")
        else:
            print("Language not supported")

    def __callback(self, ch, method, properties, body):
        """
        Execute after receiving a message from the queue

        Args:
            body:
                The message body. It is a JSON string that contains the following fields:
                    - code: The code to be executed
                    - language: The language of the code
                    - input: The input to the code
                    - test_cases: The test cases to be executed
                    - submission_id: The id of the submission
        Returns:
            None
        Raises:
            None
        """
        data = json.loads(body.decode())
        # Save the code, input to a file
        self.save_code(data["code"], data["language"])
        # handle multiple input file
        submission = {
            "stdout": [],
            "stderr": [],
            "test_cases": data["test_cases"]
        }
        for code_input in data["input"]:
            self.__save_input(code_input)
            stdout, stderr = self.__execute(data["language"])
            submission["stderr"].append(stderr)
            submission["stdout"].append(stdout)
        submission["status"] = "done execution"
        # Send the output to judge
        self.__judge(data["submission_id"])
        # Save the result to submission database
        self.redis_command(self.redis.set,
                        data["submission_id"], json.dumps(submission), 600)
        # Clean up
        self.__clean_up(data["language"])
        ch.basic_ack(delivery_tag=method.delivery_tag)

    def __judge(self, submission_id: str) -> int:
        result = requests.post(
            "http://judge.judge.svc.cluster.local/judge", json={"submission_id": submission_id})
        return result.status_code

    def __init_redis(self, environment):
        if (environment == "development"):
            redis_sentinels = os.getenv("REDIS_SENTINELS").strip()
            redis_master_name = os.environ.get('REDIS_MASTER_NAME').strip()
            redis_password = os.environ.get('REDIS_PASSWORD').strip()
            redis_sentinel = Sentinel([(redis_sentinels, 5000)], socket_timeout=5)
            redis_master = redis_sentinel.master_for(
                redis_master_name, password=redis_password, socket_timeout=5)
            return redis_master
        elif (environment == "production"):
            redis_master = redis.Redis(
                host=os.getenv("REDIS_HOST").strip(), port=6379)
            return redis_master

    def __dind(self) -> docker.DockerClient:
        max_retries = 4
        count = 0
        backoffSeconds = 2
        while True:
            try:
                dind = docker.from_env()
                # self.__load_interpreter(dind=dind, languages=self.languages)
                return dind
            except Exception as e:
                count += 1
                if count > max_retries:
                    raise
            time.sleep(backoffSeconds)

    def redis_command(self, command, *args):
        max_retries = 2
        count = 0
        backoffSeconds = 2
        while True:
            try:
                return command(*args)
            except (redis.exceptions.ConnectionError, redis.exceptions.TimeoutError):
                count += 1
                if count > max_retries:
                    raise
            print('Retrying in {} seconds'.format(backoffSeconds))
            time.sleep(backoffSeconds)

    def __init_rabbitmq(self, environment):
        if (environment == "development"):
            submission_queue = os.getenv("SUBMISSION_QUEUE").strip()
            connection = pika.BlockingConnection(
                pika.ConnectionParameters(host=submission_queue))
            channel = connection.channel()
            return channel
        elif (environment == "production"):
            rabbitmq_url = self.__inject_username_password_to_rabbitmq_url(
                os.getenv("SUBMISSION_QUEUE").strip())
            parameters = pika.URLParameters(rabbitmq_url)
            connection = pika.BlockingConnection(parameters)
            channel = connection.channel()
            return channel

    def __inject_username_password_to_rabbitmq_url(rabbitmq_url, rabbitmq_username, rabbitmq_password):
        username = rabbitmq_username.strip()
        password = rabbitmq_password.strip()
        rabbitmq_url = rabbitmq_url.replace(
            "amqps://", "amqps://{}:{}@".format(username, password))
        return rabbitmq_url

    def __load_interpreter(self, dind: docker.DockerClient, languages: dict[str, str]):
        images = [image.tags[0] for image in dind.images.list()]
        try:
            for language in languages:
                if languages[language] not in images:
                    dind.images.pull(languages[language])
        except Exception as e:
            raise e
        return

    def environment(callback):
        def wrapper(*args, **kwargs):
            try:

                callback(*args, **kwargs)
            except Exception as e:
                raise e
        return wrapper


if __name__ == "__main__":
    WORK_DIR = os.getenv("WORK_DIR")
    NODEJS_CEE_INTERPRETER_IMAGE = os.getenv(
        "NODEJS_CEE_INTERPRETER_IMAGE") or "node:16.13.0-alpine3.14"
    PYTHON_CEE_INTERPRETER_IMAGE = os.getenv(
        "PYTHON_CEE_INTERPRETER_IMAGE") or "python:alpine3.16"
    worker = Worker(
        envrionment=os.getenv("ENVIRONMENT"),
        python=PYTHON_CEE_INTERPRETER_IMAGE,
        nodejs=NODEJS_CEE_INTERPRETER_IMAGE)
    print("initialize 1")
    worker.run(os.getenv("CEE_INTERPRETER_QUEUE_NAME").strip())
    print("initialize 2")
