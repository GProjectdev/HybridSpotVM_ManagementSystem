import json
import unittest
from unittest.mock import Mock
from system_preflight import ReadOnly, inspect_clusters, inspect_fis


class PreflightTests(unittest.TestCase):
    def test_wrong_account_stops_before_permission_queries(self):
        reader = Mock(); reader.json.return_value = {'Account': '111111111111'}
        with self.assertRaisesRegex(ValueError, 'account mismatch'):
            inspect_fis(reader, 'explicit', '222222222222', 'ap-northeast-2', 'owned')
        self.assertEqual(reader.json.call_count, 1)

    def test_missing_get_stop_permission_fails_without_mutation(self):
        reader = Mock()
        def respond(args):
            self.assertNotIn('start-experiment', args)
            self.assertNotIn('put-user-policy', args)
            self.assertNotIn('create-experiment-template', args)
            if 'get-caller-identity' in args:
                return {'Account': '222222222222', 'Arn': 'arn:aws:iam::222222222222:user/test'}
            actions = args[args.index('--action-names')+1:args.index('--resource-arns')]
            return {'EvaluationResults': [{'EvalActionName': a, 'EvalDecision':
                    'implicitDeny' if a in ('fis:GetExperiment', 'fis:StopExperiment') else 'allowed'} for a in actions]}
        reader.json.side_effect = respond
        checks = inspect_fis(reader, 'explicit', '222222222222', 'ap-northeast-2', 'owned')
        self.assertEqual({c['check'] for c in checks if not c['passed']}, {'fis:GetExperiment', 'fis:StopExperiment'})

    def test_api_failure_never_serializes_raw_error_stderr(self):
        run = Mock(return_value=Mock(returncode=1, stderr='credential-private-detail', stdout=''))
        with self.assertRaisesRegex(RuntimeError, '^QUERY_UNAVAILABLE$'):
            ReadOnly(run).json(['kubectl', 'get', 'nodes'])

    def test_start_requires_both_template_and_new_experiment_resource_permission(self):
        reader = Mock()
        def respond(args):
            if 'get-caller-identity' in args:
                return {'Account': '222222222222', 'Arn': 'arn:aws:iam::222222222222:user/test'}
            resource=args[args.index('--resource-arns')+1]
            actions=args[args.index('--action-names')+1:args.index('--resource-arns')]
            if resource.endswith(':experiment/*'):
                contexts=json.loads(args[args.index('--context-entries')+1])
                self.assertEqual([x['ContextKeyName'] for x in contexts],
                                 ['aws:'+('RequestTag' if 'fis:StartExperiment' in actions else 'ResourceTag')+'/HybridSpotValidationRun'])
            return {'EvaluationResults': [{'EvalActionName': a, 'EvalDecision':
                    'implicitDeny' if resource.endswith(':experiment/*') and a=='fis:StartExperiment'
                    else 'allowed'} for a in actions]}
        reader.json.side_effect=respond
        checks=inspect_fis(reader,'explicit','222222222222','ap-northeast-2','owned')
        start=[c for c in checks if c['check']=='fis:StartExperiment']
        self.assertEqual([c['passed'] for c in start],[True,False])

    def test_api_up_with_unready_node_is_not_ready(self):
        reader = Mock()
        reader.kub.side_effect = [{}, {'items': [{'status': {'conditions': [{'type': 'Ready', 'status': 'False'}]}}]}]
        checks = inspect_clusters(reader, {'onpre1': '/private/config'})
        self.assertTrue(checks[0]['passed']); self.assertFalse(checks[1]['passed'])
